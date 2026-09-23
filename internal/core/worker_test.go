package core_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

type emission struct {
	text string
	ack  chan error
}
type finish struct {
	result core.Result
	err    error
}
type invocation struct {
	ctx    context.Context
	input  core.InferenceInput
	emit   chan emission
	finish chan finish
}
type fake struct {
	calls       chan *invocation
	mu          sync.Mutex
	active, max int
	all         []*invocation
	stopping    bool
}

func newFake() *fake { return &fake{calls: make(chan *invocation, 100)} }
func (f *fake) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "fake", Capabilities: core.Capabilities{TextGeneration: true}}}, nil
}
func (f *fake) Generate(ctx context.Context, in core.InferenceInput, emit func(string) error) (core.Result, error) {
	f.mu.Lock()
	if f.stopping {
		f.mu.Unlock()
		return core.Result{}, context.Canceled
	}
	f.active++
	f.max = max(f.max, f.active)
	f.mu.Unlock()
	c := &invocation{ctx: ctx, input: in, emit: make(chan emission), finish: make(chan finish, 1)}
	f.mu.Lock()
	f.all = append(f.all, c)
	f.mu.Unlock()
	f.calls <- c
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	for {
		select {
		case e := <-c.emit:
			e.ack <- emit(e.text)
		case r := <-c.finish:
			return r.result, r.err
		}
	}
}
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func nextCall(t *testing.T, ctx context.Context, f *fake) *invocation {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-ctx.Done():
		t.Fatal("backend call deadline")
		return nil
	}
}
func emit(t *testing.T, ctx context.Context, c *invocation, text string) error {
	t.Helper()
	ack := make(chan error, 1)
	select {
	case c.emit <- emission{text, ack}:
	case <-ctx.Done():
		t.Fatal("emit deadline")
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		t.Fatal("emit ack deadline")
		return nil
	}
}
func command(key string) core.SubmitCommand {
	return core.SubmitCommand{Key: key, Request: core.Request{Model: "fake", Role: "  role  ", SystemPrompt: " sys ", Prompt: key, Options: core.GenerationOptions{MaxOutputTokens: 8}}}
}
func setup(t *testing.T) (*core.Worker, *fake, context.Context, string) {
	t.Helper()
	ctx := testContext(t)
	dir := t.TempDir()
	s, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	w, err := core.NewWorker(s, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithCancel(context.Background())
		cancel()
		_ = w.Shutdown(stop)
		f.mu.Lock()
		f.stopping = true
		for _, c := range f.all {
			select {
			case c.finish <- finish{}:
			default:
			}
		}
		f.mu.Unlock()
		cleanup, release := context.WithTimeout(context.Background(), 20*time.Second)
		defer release()
		if err := w.Shutdown(cleanup); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return w, f, ctx, dir
}
func submit(t *testing.T, w *core.Worker, ctx context.Context, key string) core.Job {
	t.Helper()
	j, err := w.Submit(ctx, command(key))
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func waitState(t *testing.T, w *core.Worker, ctx context.Context, id core.JobID, state core.JobState) core.Job {
	t.Helper()
	s, err := w.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range s.Jobs {
		if j.ID == id && j.State == state {
			return j
		}
	}
	stream, err := w.Watch(ctx, s.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		e, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e.Job != nil && e.JobID == id && e.Job.State == state {
			return *e.Job
		}
	}
}
func code(t *testing.T, err error, want core.ErrorCode) {
	t.Helper()
	var f *core.Fault
	if !errors.As(err, &f) || f.Code != want {
		t.Fatalf("error %v; want %s", err, want)
	}
}

func TestFIFOOneActivePauseAndClientDetachment(t *testing.T) {
	w, f, ctx, _ := setup(t)
	w.SetQueuePaused(ctx, true)
	client, cancel := context.WithCancel(ctx)
	a := submit(t, w, client, "a")
	b := submit(t, w, ctx, "b")
	cancel()
	s, _ := w.Snapshot(ctx)
	if !s.Queue.Paused || s.Queue.Active != nil || len(s.Queue.Pending) != 2 {
		t.Fatal(s.Queue)
	}
	w.SetQueuePaused(ctx, false)
	ca := nextCall(t, ctx, f)
	if ca.input.Instructions.Prompt != "a" || ca.ctx.Err() != nil {
		t.Fatal("wrong or detached inference")
	}
	w.SetQueuePaused(ctx, true)
	if err := emit(t, ctx, ca, "part"); err != nil {
		t.Fatal(err)
	}
	ca.finish <- finish{result: core.Result{Text: "must not append", FinishReason: "stop"}}
	got := waitState(t, w, ctx, a.ID, core.JobSucceeded)
	if got.Result.Text != "part" {
		t.Fatal(got.Result)
	}
	s, _ = w.Snapshot(ctx)
	if s.Queue.Active != nil || len(s.Queue.Pending) != 1 {
		t.Fatal(s.Queue)
	}
	w.SetQueuePaused(ctx, false)
	cb := nextCall(t, ctx, f)
	if cb.input.Instructions.Prompt != "b" {
		t.Fatal("not FIFO")
	}
	cb.finish <- finish{}
	waitState(t, w, ctx, b.ID, core.JobSucceeded)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.max != 1 {
		t.Fatal("overlapping inference")
	}
}

func TestCancellationOrdering(t *testing.T) {
	w, f, ctx, _ := setup(t)
	a := submit(t, w, ctx, "a")
	ca := nextCall(t, ctx, f)
	b := submit(t, w, ctx, "b")
	j, err := w.Cancel(ctx, b.ID)
	if err != nil || j.State != core.JobCancelled {
		t.Fatal(j, err)
	}
	j, err = w.Cancel(ctx, a.ID)
	if err != nil || j.State != core.JobCancelling {
		t.Fatal(j, err)
	}
	select {
	case <-ca.ctx.Done():
	case <-ctx.Done():
		t.Fatal("not cancelled")
	}
	c := submit(t, w, ctx, "c")
	s, _ := w.Snapshot(ctx)
	if s.Queue.Active == nil || *s.Queue.Active != a.ID {
		t.Fatal("released before backend return")
	}
	if err := emit(t, ctx, ca, "last"); err != nil {
		t.Fatal(err)
	}
	ca.finish <- finish{}
	j = waitState(t, w, ctx, a.ID, core.JobCancelled)
	if j.Result.Text != "last" {
		t.Fatal(j)
	}
	cc := nextCall(t, ctx, f)
	cc.finish <- finish{}
	waitState(t, w, ctx, c.ID, core.JobSucceeded)
	j, err = w.Cancel(ctx, c.ID)
	if err != nil || j.State != core.JobSucceeded {
		t.Fatal("committed completion must win")
	}
	j, err = w.Cancel(ctx, a.ID)
	if err != nil || j.State != core.JobCancelled {
		t.Fatal("cancel not idempotent")
	}
}

func TestLineageIsolationAndOwnership(t *testing.T) {
	w, f, ctx, _ := setup(t)
	temp := 0.5
	cmd := command("original")
	cmd.Request.Options.Temperature = &temp
	p, err := w.Submit(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	temp = 99
	p.Request.Options.MaxOutputTokens = 999
	c := nextCall(t, ctx, f)
	if c.input.Instructions.System != "role\n\nsys" || len(c.input.Instructions.History) != 0 || *c.input.Request.Options.Temperature != 0.5 {
		t.Fatal(c.input)
	}
	c.input.Request.Options.MaxOutputTokens = 999
	emit(t, ctx, c, "partial")
	c.finish <- finish{err: errors.New("secret")}
	p = waitState(t, w, ctx, p.ID, core.JobFailed)
	r, err := w.Retry(ctx, core.RetryCommand{Key: "retry", ParentID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	cr := nextCall(t, ctx, f)
	if cr.input.Request.Options.MaxOutputTokens != 8 || len(cr.input.Instructions.History) != 0 || r.Result.Text != "" || r.Lineage.Relation != core.RelationRetry {
		t.Fatal(r)
	}
	cr.finish <- finish{}
	waitState(t, w, ctx, r.ID, core.JobSucceeded)
	b, err := w.Branch(ctx, core.BranchCommand{Key: "branch", ParentID: p.ID, Request: command("continue").Request})
	if err != nil {
		t.Fatal(err)
	}
	cb := nextCall(t, ctx, f)
	if len(cb.input.Instructions.History) != 2 || cb.input.Instructions.History[1].Content != "partial" {
		t.Fatal(cb.input)
	}
	cb.finish <- finish{}
	waitState(t, w, ctx, b.ID, core.JobSucceeded)
	fresh := submit(t, w, ctx, "fresh")
	cf := nextCall(t, ctx, f)
	if len(cf.input.Instructions.History) != 0 {
		t.Fatal("history leaked")
	}
	cf.finish <- finish{}
	waitState(t, w, ctx, fresh.ID, core.JobSucceeded)
	s, _ := w.Snapshot(ctx)
	s.Jobs[0].Error.Details = map[string]string{"mutated": "yes"}
	s.Jobs[0].Instructions.History = append(s.Jobs[0].Instructions.History, core.Message{})
	s.Jobs[0].Request.Options.Temperature = &temp
	original, _ := w.GetJob(ctx, p.ID)
	if original.Error.Message == "secret" || original.Error.Details != nil || len(original.Instructions.History) != 0 || *original.Request.Options.Temperature != 0.5 {
		t.Fatal("snapshot or error isolation failed")
	}
}

func TestQueueBoundIdempotencyAndRestart(t *testing.T) {
	w, _, ctx, dir := setup(t)
	w.SetQueuePaused(ctx, true)
	a := submit(t, w, ctx, "a")
	for i := 1; i < core.MaxPending; i++ {
		submit(t, w, ctx, fmt.Sprint(i))
	}
	_, err := w.Submit(ctx, command("overflow"))
	code(t, err, core.ErrQueueFull)
	again, err := w.Submit(ctx, command("a"))
	if err != nil || again.ID != a.ID {
		t.Fatal(again, err)
	}
	changed := command("a")
	changed.Request.Prompt = "changed"
	_, err = w.Submit(ctx, changed)
	code(t, err, core.ErrConflict)
	if err = w.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := core.NewWorker(s, newFake())
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Shutdown(ctx)
	again, err = w2.Submit(ctx, command("a"))
	if err != nil || again.ID != a.ID {
		t.Fatal(again, err)
	}
	snap, _ := w2.Snapshot(ctx)
	if !snap.Queue.Paused || len(snap.Queue.Pending) != core.MaxPending {
		t.Fatal(snap.Queue)
	}
}

func TestSnapshotReplayLiveAndSlowObserver(t *testing.T) {
	w, f, ctx, _ := setup(t)
	base, _ := w.Snapshot(ctx)
	a := submit(t, w, ctx, "a")
	c := nextCall(t, ctx, f)
	emit(t, ctx, c, "αβ")
	snap, _ := w.Snapshot(ctx)
	replay, err := w.Watch(ctx, base.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	live, err := w.Watch(ctx, snap.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	emit(t, ctx, c, "γ")
	e, err := live.Next(ctx)
	if err != nil || e.Output == nil || e.Output.OffsetBytes != 4 {
		t.Fatal(e, err)
	}
	end := e.Cursor.Sequence
	for n := uint64(1); n <= end; n++ {
		e, err = replay.Next(ctx)
		if err != nil || e.Cursor.Sequence != n {
			t.Fatal(e, err)
		}
	}
	reconnect, err := w.Watch(ctx, snap.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	same, err := reconnect.Next(ctx)
	if err != nil || same.Cursor.Sequence != end {
		t.Fatal("reconnect replay")
	}
	reconnect.Close()
	_, err = w.Watch(ctx, core.Cursor{StoreID: "other"})
	code(t, err, core.ErrCursorInvalid)
	future := snap.Cursor
	future.Sequence = 999999
	_, err = w.Watch(ctx, future)
	code(t, err, core.ErrCursorInvalid)
	slow, _ := w.Watch(ctx, snap.Cursor)
	live.Close()
	_, err = live.Next(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for i := 0; i <= core.MaxObserverEvents; i++ {
		if _, err = w.SetQueuePaused(ctx, i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	_, err = slow.Next(ctx)
	code(t, err, core.ErrSlowConsumer)
	c.finish <- finish{}
	waitState(t, w, ctx, a.ID, core.JobSucceeded)
}

func TestShutdownKeepsOwnershipUntilBackendReturns(t *testing.T) {
	w, f, ctx, dir := setup(t)
	a := submit(t, w, ctx, "a")
	c := nextCall(t, ctx, f)
	emit(t, ctx, c, "partial")
	b := submit(t, w, ctx, "b")
	expired, cancel := context.WithCancel(ctx)
	cancel()
	if err := w.Shutdown(expired); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s, err := journal.Open(dir); err == nil {
		s.Close()
		t.Fatal("ownership released while backend active")
	}
	select {
	case <-c.ctx.Done():
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel")
	}
	c.finish <- finish{}
	if err := w.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ := w.Snapshot(ctx)
	for _, j := range s.Jobs {
		if j.ID == a.ID && (j.State != core.JobInterrupted || j.Result.Text != "partial") {
			t.Fatal(j)
		}
		if j.ID == b.ID && j.State != core.JobQueued {
			t.Fatal(j)
		}
	}
	store, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err := store.Recover(ctx)
	if err != nil || r.Snapshot.Cursor != s.Cursor {
		t.Fatal(r, err)
	}
}

func TestValidationAndOutputBounds(t *testing.T) {
	w, f, _, _ := setup(t)
	// Filling the output bound takes about 7s, and about 83s under the race detector, so this test gets
	// its own long limit; the shared 20s limit still catches a hang in every other test.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	cases := []core.SubmitCommand{command("a"), command("b"), command("c"), command("d")}
	cases[0].Request.Model = " "
	cases[1].Request.Prompt = "\xff"
	cases[2].Request.Options.MaxOutputTokens = 0
	cases[3].Key = ""
	for _, c := range cases {
		_, err := w.Submit(ctx, c)
		code(t, err, core.ErrInvalidRequest)
	}
	j := submit(t, w, ctx, "output")
	c := nextCall(t, ctx, f)
	// JSON control characters expand sixfold: encoded terminal capacity is reached
	// before the raw output limit, and the already committed prefix is retained.
	chunk := strings.Repeat("\x00", core.MaxChunkBytes)
	var err error
	for i := 0; i < core.MaxOutputBytes/core.MaxChunkBytes; i++ {
		err = emit(t, ctx, c, chunk)
		if err != nil {
			break
		}
	}
	code(t, err, core.ErrLimitExceeded)
	c.finish <- finish{}
	j = waitState(t, w, ctx, j.ID, core.JobFailed)
	if len(j.Result.Text) == 0 || len(j.Result.Text) >= core.MaxOutputBytes || j.Error.Code != core.ErrLimitExceeded {
		t.Fatal("encoded bound not enforced")
	}
}

type failingStore struct {
	core.Store
	fail bool
}

func (s *failingStore) Append(ctx context.Context, c core.Commit) error {
	if s.fail {
		return &core.Fault{Code: core.ErrStorage, Message: "injected sync ambiguity"}
	}
	return s.Store.Append(ctx, c)
}
func TestStorageFailureDoesNotPublishOrDispatch(t *testing.T) {
	ctx := testContext(t)
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &failingStore{Store: j}
	f := newFake()
	w, err := core.NewWorker(s, f)
	if err != nil {
		t.Fatal(err)
	}
	w.SetQueuePaused(ctx, true)
	before, _ := w.Snapshot(ctx)
	stream, _ := w.Watch(ctx, before.Cursor)
	s.fail = true
	_, err = w.Submit(ctx, command("failure"))
	code(t, err, core.ErrStorage)
	after, _ := w.Snapshot(ctx)
	if after.Cursor != before.Cursor || len(after.Jobs) != 0 {
		t.Fatal("published failed commit")
	}
	_, err = stream.Next(ctx)
	code(t, err, core.ErrStorage)
	code(t, w.Shutdown(ctx), core.ErrStorage)
}

func TestConcurrentCompletionAndCancel(t *testing.T) {
	w, f, ctx, _ := setup(t)
	for i := 0; i < 20; i++ {
		j := submit(t, w, ctx, fmt.Sprintf("race-%d", i))
		c := nextCall(t, ctx, f)
		start := make(chan struct{})
		cancelled := make(chan error, 1)
		go func() { <-start; _, err := w.Cancel(ctx, j.ID); cancelled <- err }()
		go func() { <-start; c.finish <- finish{} }()
		close(start)
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
		snap, _ := w.Snapshot(ctx)
		watch, err := w.Watch(ctx, snap.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := w.GetJob(ctx, j.ID)
		for !core.Terminal(got.State) {
			e, err := watch.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if e.JobID == j.ID && e.Job != nil {
				got = *e.Job
			}
		}
		watch.Close()
		if got.State != core.JobCancelled && got.State != core.JobSucceeded {
			t.Fatal(got.State)
		}
	}
}

type gatedReplayStore struct {
	core.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedReplayStore) ReadEvents(ctx context.Context, c core.Cursor, n int) ([]core.Event, error) {
	s.once.Do(func() { close(s.entered); <-s.release })
	return s.Store.ReadEvents(ctx, c, n)
}
func TestReplayLiveHandoffWhileReplayIsBlocked(t *testing.T) {
	ctx := testContext(t)
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &gatedReplayStore{Store: j, entered: make(chan struct{}), release: make(chan struct{})}
	w, err := core.NewWorker(s, newFake())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Shutdown(ctx)
	base, _ := w.Snapshot(ctx)
	w.SetQueuePaused(ctx, true)
	submit(t, w, ctx, "queued")
	boundary, _ := w.Snapshot(ctx)
	watch, err := w.Watch(ctx, base.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	got := make(chan core.Event, 1)
	failed := make(chan error, 1)
	go func() { e, err := watch.Next(ctx); got <- e; failed <- err }()
	select {
	case <-s.entered:
	case <-ctx.Done():
		t.Fatal("replay did not start")
	}
	submit(t, w, ctx, "during-replay")
	end, _ := w.Snapshot(ctx)
	if end.Cursor.Sequence <= boundary.Cursor.Sequence {
		t.Fatal("live commit did not proceed")
	}
	close(s.release)
	first := <-got
	if err := <-failed; err != nil {
		t.Fatal(err)
	}
	if first.Cursor.Sequence != 1 {
		t.Fatal(first)
	}
	for n := uint64(2); n <= end.Cursor.Sequence; n++ {
		e, err := watch.Next(ctx)
		if err != nil || e.Cursor.Sequence != n {
			t.Fatal("handoff gap", e, err)
		}
	}
}

func TestWatchContextDetachesOnlyObserver(t *testing.T) {
	w, f, ctx, _ := setup(t)
	j := submit(t, w, ctx, "detached-watch")
	c := nextCall(t, ctx, f)
	snap, _ := w.Snapshot(ctx)
	watchCtx, cancel := context.WithCancel(ctx)
	watch, err := w.Watch(watchCtx, snap.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = watch.Next(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if c.ctx.Err() != nil {
		t.Fatal("watch cancelled inference")
	}
	c.finish <- finish{}
	waitState(t, w, ctx, j.ID, core.JobSucceeded)
}

func TestBackendMetadataAndInvalidOutput(t *testing.T) {
	w, f, ctx, _ := setup(t)
	models, err := w.Models(ctx)
	if err != nil || len(models) != 1 || !models[0].Capabilities.TextGeneration {
		t.Fatal(models, err)
	}
	for _, mode := range []string{"metadata", "utf8"} {
		j := submit(t, w, ctx, mode)
		c := nextCall(t, ctx, f)
		emit(t, ctx, c, "retained")
		if mode == "metadata" {
			c.finish <- finish{result: core.Result{FinishReason: strings.Repeat("x", core.MetadataReserve)}}
		} else {
			code(t, emit(t, ctx, c, "\xff"), core.ErrBackendFailure)
			c.finish <- finish{}
		}
		got := waitState(t, w, ctx, j.ID, core.JobFailed)
		if got.Result.Text != "retained" || got.Error.Code != core.ErrBackendFailure {
			t.Fatal(got)
		}
	}
}

func TestStorageFailureCancelsActiveAndPreventsNextDispatch(t *testing.T) {
	ctx := testContext(t)
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &failingStore{Store: j}
	f := newFake()
	w, err := core.NewWorker(s, f)
	if err != nil {
		t.Fatal(err)
	}
	a := submit(t, w, ctx, "a")
	c := nextCall(t, ctx, f)
	b := submit(t, w, ctx, "b")
	before, _ := w.Snapshot(ctx)
	s.fail = true
	code(t, emit(t, ctx, c, "not durable"), core.ErrStorage)
	select {
	case <-c.ctx.Done():
	case <-ctx.Done():
		t.Fatal("storage error did not cancel active inference")
	}
	c.finish <- finish{}
	code(t, w.Shutdown(ctx), core.ErrStorage)
	after, _ := w.Snapshot(ctx)
	if after.Cursor != before.Cursor {
		t.Fatal("failed output published")
	}
	for _, j := range after.Jobs {
		if j.ID == a.ID && (j.State != core.JobRunning || j.Result.Text != "") {
			t.Fatal(j)
		}
		if j.ID == b.ID && j.State != core.JobQueued {
			t.Fatal(j)
		}
	}
}

type gatedAppendStore struct {
	core.Store
	entered chan struct{}
	release chan struct{}
}

func (s *gatedAppendStore) Append(ctx context.Context, c core.Commit) error {
	if c.Receipt != nil {
		select {
		case <-s.entered:
		default:
			close(s.entered)
			<-s.release
		}
	}
	return s.Store.Append(ctx, c)
}
func TestCallerDeadlineDuringCommitDoesNotUndoAcceptance(t *testing.T) {
	ctx := testContext(t)
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &gatedAppendStore{Store: j, entered: make(chan struct{}), release: make(chan struct{})}
	w, err := core.NewWorker(s, newFake())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Shutdown(ctx)
	w.SetQueuePaused(ctx, true)
	client, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	cmd := command("ambiguous-response")
	temp := 0.25
	cmd.Request.Options.Temperature = &temp
	go func() { _, err := w.Submit(client, cmd); result <- err }()
	select {
	case <-s.entered:
	case <-ctx.Done():
		t.Fatal("append not entered")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("caller stayed blocked by commit")
	}
	// The command owns its option values even after a detached caller changes them.
	temp = 0.75
	readCtx, readCancel := context.WithCancel(ctx)
	readCancel()
	if _, err = w.Snapshot(readCtx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(s.release)
	original := command("ambiguous-response")
	originalTemp := 0.25
	original.Request.Options.Temperature = &originalTemp
	accepted, err := w.Submit(ctx, original)
	if err != nil || accepted.State != core.JobQueued || *accepted.Request.Options.Temperature != 0.25 {
		t.Fatal(accepted, err)
	}
	snap, _ := w.Snapshot(ctx)
	if len(snap.Jobs) != 1 {
		t.Fatal("duplicate acceptance")
	}
}
