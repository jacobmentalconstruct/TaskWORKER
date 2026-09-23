package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"
	"unicode/utf8"
)

// Worker serializes mutations and backend notifications through mu. Store calls
// use service-owned contexts: caller detachment cannot undo durable acceptance.
type Worker struct {
	mu        sync.Mutex
	store     Store
	backend   Backend
	state     Snapshot
	receipts  map[string]Receipt
	observers map[*stream]bool
	cancel    context.CancelFunc
	outputErr error
	fatal     error
	stopping  bool
	done      chan struct{}
	closed    bool
}

var _ Service = (*Worker)(nil)

func NewWorker(store Store, backend Backend) (*Worker, error) {
	if store == nil || backend == nil {
		return nil, fault(ErrInvalidRequest, "store and backend required")
	}
	r, err := store.Recover(context.Background())
	if err != nil {
		return nil, err
	}
	w := &Worker{store: store, backend: backend, state: r.Snapshot, receipts: map[string]Receipt{}, observers: map[*stream]bool{}, done: make(chan struct{})}
	for _, v := range r.Receipts {
		w.receipts[v.Key] = v
	}
	if w.state.Queue.Active != nil {
		j := Clone(*w.job(*w.state.Queue.Active))
		j.State = JobInterrupted
		j.FinishedAt = nowPtr()
		j.Error = fault(ErrInterrupted, "service stopped during inference")
		q := Clone(w.state.Queue)
		q.Active = nil
		if err = w.commit([]Event{jobEvent(EventJobResult, j), queueEvent(q)}, nil); err != nil {
			return nil, err
		}
	}
	w.mu.Lock()
	w.dispatch()
	err = w.fatal
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return w, nil
}

func nowPtr() *time.Time                { n := time.Now().UTC(); return &n }
func jobEvent(k EventKind, j Job) Event { return Event{Kind: k, JobID: j.ID, Job: &j} }
func queueEvent(q QueueState) Event     { return Event{Kind: EventQueueState, Queue: &q} }
func (w *Worker) job(id JobID) *Job {
	for i := range w.state.Jobs {
		if w.state.Jobs[i].ID == id {
			return &w.state.Jobs[i]
		}
	}
	return nil
}
func (w *Worker) available() error {
	if w.fatal != nil {
		return w.fatal
	}
	if w.stopping {
		return fault(ErrUnavailable, "worker is shutting down")
	}
	return nil
}

func (w *Worker) poison(err error) {
	w.fatal = err
	if w.cancel != nil {
		w.cancel()
	}
	for s := range w.observers {
		s.finish(err)
	}
	clear(w.observers)
}
func (w *Worker) commit(events []Event, receipt *Receipt) error {
	seq := w.state.Cursor.Sequence
	for i := range events {
		if seq == math.MaxUint64 {
			err := fault(ErrStorage, "event sequence exhausted")
			w.poison(err)
			return err
		}
		seq++
		events[i].Cursor = Cursor{StoreID: w.state.Cursor.StoreID, Sequence: seq}
		events[i].At = time.Now().UTC()
	}
	c := Commit{Version: 1, Events: events, Receipt: receipt}
	next, err := ApplyCommit(w.state, c)
	if err != nil {
		w.poison(err)
		return err
	}
	if err = w.store.Append(context.Background(), c); err != nil {
		var f *Fault
		if !errors.As(err, &f) || f.Code != ErrLimitExceeded {
			w.poison(err)
		}
		return err
	}
	w.state = next
	if receipt != nil {
		w.receipts[receipt.Key] = *receipt
	}
	for _, e := range events {
		b, _ := json.Marshal(e)
		for s := range w.observers {
			if !s.push(e, len(b)) {
				delete(w.observers, s)
			}
		}
	}
	return nil
}

func (w *Worker) Submit(ctx context.Context, c SubmitCommand) (Job, error) {
	c.Request = copyRequest(c.Request)
	return call(ctx, func() (Job, error) { return w.create(ctx, "submit", c.Key, c.Origin, "", c.Request) })
}
func (w *Worker) Retry(ctx context.Context, c RetryCommand) (Job, error) {
	return call(ctx, func() (Job, error) { return w.create(ctx, "retry", c.Key, c.Origin, c.ParentID, Request{}) })
}
func (w *Worker) Branch(ctx context.Context, c BranchCommand) (Job, error) {
	c.Request = copyRequest(c.Request)
	return call(ctx, func() (Job, error) { return w.create(ctx, "branch", c.Key, c.Origin, c.ParentID, c.Request) })
}

func copyPointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
func copyRequest(r Request) Request {
	r.Options.ContextTokens = copyPointer(r.Options.ContextTokens)
	r.Options.Temperature = copyPointer(r.Options.Temperature)
	r.Options.Seed = copyPointer(r.Options.Seed)
	return r
}

// Once a command enters a durable commit it must finish independently of its
// caller. A buffered reply lets the caller stop waiting even during File.Sync.
func call[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type response struct {
		value T
		err   error
	}
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	reply := make(chan response, 1)
	go func() { v, err := fn(); reply <- response{v, err} }()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case r := <-reply:
		return r.value, r.err
	}
}

func (w *Worker) create(ctx context.Context, op, key, origin string, parent JobID, r Request) (Job, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if err := w.available(); err != nil {
		return Job{}, err
	}
	if len(key) == 0 || len(key) > 128 || !utf8.ValidString(key) || !utf8.ValidString(origin) || len(origin) > 1024 {
		return Job{}, fault(ErrInvalidRequest, "invalid idempotency key or origin")
	}
	// Bound strings before allocating the canonical digest encoding.
	if op != "retry" {
		if err := ValidateRequest(r, Compose(r)); err != nil {
			return Job{}, err
		}
	}
	canonical := struct {
		Operation string  `json:"operation"`
		Parent    JobID   `json:"parent"`
		Origin    string  `json:"origin"`
		Request   Request `json:"request"`
	}{op, parent, origin, r}
	b, err := json.Marshal(canonical)
	if err != nil {
		return Job{}, fault(ErrInvalidRequest, "invalid command encoding")
	}
	digest := sha256.Sum256(b)
	d := hex.EncodeToString(digest[:])
	if receipt, ok := w.receipts[key]; ok {
		if receipt.Digest != d {
			return Job{}, fault(ErrConflict, "idempotency key reused for different content")
		}
		return Clone(*w.job(receipt.JobID)), nil
	}
	in := Compose(r)
	var lineage *Lineage
	if parent != "" {
		p := w.job(parent)
		if p == nil {
			return Job{}, fault(ErrNotFound, "parent not found")
		}
		if !Terminal(p.State) {
			return Job{}, fault(ErrConflict, "parent must be terminal")
		}
		lineage = &Lineage{ParentID: parent, Relation: Relation(op)}
		if op == "retry" {
			r = Clone(p.Request)
			in = Clone(p.Instructions)
		} else {
			in.History = append(in.History, p.Instructions.History...)
			in.History = append(in.History, Message{Role: MessageUser, Content: p.Instructions.Prompt})
			if p.Result.Text != "" {
				in.History = append(in.History, Message{Role: MessageAssistant, Content: p.Result.Text})
			}
		}
	} else if op != "submit" {
		return Job{}, fault(ErrInvalidRequest, "parent is required")
	}
	if err = ValidateRequest(r, in); err != nil {
		return Job{}, err
	}
	if len(w.state.Queue.Pending) >= MaxPending {
		return Job{}, fault(ErrQueueFull, "pending queue is full")
	}
	if len(w.state.Jobs) >= MaxJobs {
		return Job{}, fault(ErrLimitExceeded, "retained job limit reached")
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return Job{}, fault(ErrInternal, "cannot allocate job identity")
	}
	j := Job{ID: JobID(hex.EncodeToString(id)), State: JobQueued, Request: Clone(r), Instructions: Clone(in), Origin: origin, Lineage: lineage, CreatedAt: time.Now().UTC()}
	if err = CheckJobFits(j); err != nil {
		return Job{}, err
	}
	q := Clone(w.state.Queue)
	q.Pending = append(q.Pending, j.ID)
	receipt := Receipt{Key: key, Digest: d, JobID: j.ID}
	if err = w.commit([]Event{jobEvent(EventJobAccepted, j), queueEvent(q)}, &receipt); err != nil {
		return Job{}, err
	}
	w.dispatch()
	return Clone(*w.job(j.ID)), nil
}

func (w *Worker) dispatch() {
	if w.stopping || w.fatal != nil || w.state.Queue.Paused || w.state.Queue.Active != nil || len(w.state.Queue.Pending) == 0 {
		return
	}
	j := Clone(*w.job(w.state.Queue.Pending[0]))
	j.State = JobRunning
	j.StartedAt = nowPtr()
	q := Clone(w.state.Queue)
	q.Pending = q.Pending[1:]
	q.Active = &j.ID
	if err := w.commit([]Event{jobEvent(EventJobState, j), queueEvent(q)}, nil); err != nil {
		w.poison(err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.outputErr = nil
	input := Clone(InferenceInput{Request: j.Request, Instructions: j.Instructions})
	go func() {
		result, err := w.backend.Generate(ctx, input, func(text string) error { return w.emit(j.ID, text) })
		w.complete(j.ID, result, err)
	}()
}

func (w *Worker) emit(id JobID, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fatal != nil {
		return w.fatal
	}
	if w.outputErr != nil {
		return w.outputErr
	}
	if !utf8.ValidString(text) {
		w.outputErr = fault(ErrBackendFailure, "backend output is not UTF-8")
		w.cancel()
		return w.outputErr
	}
	for len(text) > 0 {
		n := min(len(text), MaxChunkBytes)
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		chunk := text[:n]
		text = text[n:]
		j := *w.job(id)
		offset := len(j.Result.Text)
		j.Result.Text += chunk
		var err error
		if len(j.Result.Text) > MaxOutputBytes {
			err = fault(ErrLimitExceeded, "output limit exceeded")
		} else {
			err = CheckJobFits(j)
		}
		if err == nil {
			err = w.commit([]Event{{Kind: EventJobOutput, JobID: id, Output: &OutputDelta{OffsetBytes: int64(offset), Text: chunk}}}, nil)
		}
		if err != nil {
			w.outputErr = err
			w.cancel()
			return err
		}
	}
	return nil
}

func (w *Worker) complete(id JobID, result Result, backendErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	if w.fatal == nil {
		j := Clone(*w.job(id))
		j.FinishedAt = nowPtr()
		// Metadata is bounded separately so a backend cannot consume terminal reserve.
		result.Text = ""
		b, err := json.Marshal(result)
		if err != nil || len(b) > MetadataReserve/2 || !utf8.ValidString(result.FinishReason) || !utf8.ValidString(result.EffectiveModel) {
			backendErr = fault(ErrBackendFailure, "invalid or oversized backend metadata")
			result = Result{}
		}
		result.Text = j.Result.Text
		j.Result = Clone(result)
		switch {
		case w.stopping:
			j.State = JobInterrupted
			j.Error = fault(ErrInterrupted, "service shutdown interrupted inference")
		case j.State == JobCancelling:
			j.State = JobCancelled
			j.Error = fault(ErrCancelled, "job cancelled")
		case w.outputErr != nil:
			j.State = JobFailed
			j.Error = publicBackendError(w.outputErr)
		case backendErr != nil:
			j.State = JobFailed
			j.Error = publicBackendError(backendErr)
		default:
			j.State = JobSucceeded
		}
		q := Clone(w.state.Queue)
		q.Active = nil
		if err = w.commit([]Event{jobEvent(EventJobResult, j), queueEvent(q)}, nil); err != nil {
			w.poison(err)
		}
	}
	if w.stopping {
		w.closeStore()
	} else {
		w.dispatch()
	}
}

func publicBackendError(err error) *Fault {
	var f *Fault
	// Only classifications cross this boundary, never backend text or details.
	// Validation/limits need intervention; only classified availability/failure
	// faults may carry the backend's retry advice. See docs/contracts.md.
	if errors.As(err, &f) && f != nil {
		switch f.Code {
		case ErrInvalidRequest:
			return fault(ErrInvalidRequest, "backend rejected the request")
		case ErrUnsupported:
			return fault(ErrUnsupported, "backend does not support the requested operation or option")
		case ErrLimitExceeded:
			return fault(ErrLimitExceeded, "backend or worker resource limit exceeded")
		case ErrBackendUnavailable:
			return &Fault{Code: ErrBackendUnavailable, Message: "backend is unavailable", Retryable: f.Retryable}
		case ErrBackendFailure:
			return &Fault{Code: ErrBackendFailure, Message: "backend operation failed", Retryable: f.Retryable}
		}
	}
	return fault(ErrBackendFailure, "backend operation failed")
}

func (w *Worker) Cancel(ctx context.Context, id JobID) (Job, error) {
	return call(ctx, func() (Job, error) { return w.cancelJob(ctx, id) })
}
func (w *Worker) cancelJob(ctx context.Context, id JobID) (Job, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if err := w.available(); err != nil {
		return Job{}, err
	}
	p := w.job(id)
	if p == nil {
		return Job{}, fault(ErrNotFound, "job not found")
	}
	j := Clone(*p)
	if Terminal(j.State) || j.State == JobCancelling {
		return j, nil
	}
	q := Clone(w.state.Queue)
	kind := EventJobState
	if j.State == JobQueued {
		j.State = JobCancelled
		j.FinishedAt = nowPtr()
		j.Error = fault(ErrCancelled, "job cancelled")
		kind = EventJobResult
		for i, v := range q.Pending {
			if v == id {
				q.Pending = append(q.Pending[:i], q.Pending[i+1:]...)
				break
			}
		}
	} else {
		j.State = JobCancelling
	}
	if err := w.commit([]Event{jobEvent(kind, j), queueEvent(q)}, nil); err != nil {
		return Job{}, err
	}
	if j.State == JobCancelling {
		w.cancel()
	}
	return Clone(j), nil
}
func (w *Worker) SetQueuePaused(ctx context.Context, paused bool) (QueueState, error) {
	return call(ctx, func() (QueueState, error) { return w.setQueuePaused(ctx, paused) })
}
func (w *Worker) setQueuePaused(ctx context.Context, paused bool) (QueueState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return QueueState{}, err
	}
	if err := w.available(); err != nil {
		return QueueState{}, err
	}
	q := Clone(w.state.Queue)
	if q.Paused != paused {
		q.Paused = paused
		if err := w.commit([]Event{queueEvent(q)}, nil); err != nil {
			return QueueState{}, err
		}
	}
	w.dispatch()
	return Clone(w.state.Queue), nil
}
func (w *Worker) GetJob(ctx context.Context, id JobID) (Job, error) {
	return call(ctx, func() (Job, error) { return w.getJob(ctx, id) })
}
func (w *Worker) getJob(ctx context.Context, id JobID) (Job, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	j := w.job(id)
	if j == nil {
		return Job{}, fault(ErrNotFound, "job not found")
	}
	return Clone(*j), nil
}
func (w *Worker) Snapshot(ctx context.Context) (Snapshot, error) {
	return call(ctx, func() (Snapshot, error) { return w.snapshot(ctx) })
}
func (w *Worker) snapshot(ctx context.Context) (Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return Clone(w.state), nil
}
func (w *Worker) Models(ctx context.Context) ([]Model, error) {
	return call(ctx, func() ([]Model, error) { return w.models(ctx) })
}
func (w *Worker) models(ctx context.Context) ([]Model, error) {
	m, err := w.backend.Models(ctx)
	if err != nil {
		return nil, publicBackendError(err)
	}
	if _, err = json.Marshal(m); err != nil {
		return nil, fault(ErrBackendFailure, "invalid model metadata")
	}
	return Clone(m), nil
}

// Shutdown requests dispatch stop, then waits for cooperative backend return.
// A deadline only stops waiting; the worker retains ownership until return.
func (w *Worker) Shutdown(ctx context.Context) error {
	go func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.stopping {
			w.stopping = true
			if w.cancel != nil {
				w.cancel()
			} else {
				w.closeStore()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.fatal
	}
}
func (w *Worker) closeStore() {
	if w.closed {
		return
	}
	for s := range w.observers {
		s.finish(nil)
	}
	clear(w.observers)
	if err := w.store.Close(); err != nil && w.fatal == nil {
		w.fatal = err
	}
	w.closed = true
	close(w.done)
}
