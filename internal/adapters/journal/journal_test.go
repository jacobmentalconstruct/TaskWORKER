package journal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/core"
)

var bg = context.Background()

func openTest(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, dir
}
func accepted(j *Journal, key string) core.Commit {
	r := core.Request{Model: "fake", Prompt: key, Options: core.GenerationOptions{MaxOutputTokens: 8}}
	job := core.Job{ID: core.JobID(key), State: core.JobQueued, Request: r, Instructions: core.Compose(r), CreatedAt: time.Now().UTC()}
	q := core.Clone(j.state.Snapshot.Queue)
	q.Pending = append(q.Pending, job.ID)
	return transaction(j, []core.Event{{Kind: core.EventJobAccepted, JobID: job.ID, Job: &job}, {Kind: core.EventQueueState, Queue: &q}}, &core.Receipt{Key: key, Digest: strings.Repeat("a", 64), JobID: job.ID})
}
func transaction(j *Journal, events []core.Event, r *core.Receipt) core.Commit {
	n := j.state.Snapshot.Cursor.Sequence
	for i := range events {
		n++
		events[i].Cursor = core.Cursor{StoreID: j.state.Snapshot.Cursor.StoreID, Sequence: n}
		events[i].At = time.Now().UTC()
	}
	return core.Commit{Version: 1, Events: events, Receipt: r}
}
func appendOK(t *testing.T, j *Journal, c core.Commit) {
	t.Helper()
	if err := j.Append(bg, c); err != nil {
		t.Fatal(err)
	}
}
func record(c core.Commit) []byte { b, _ := json.Marshal(c); return frame(b) }
func frame(b []byte) []byte {
	h := make([]byte, FrameSize)
	binary.BigEndian.PutUint32(h[:4], 1)
	binary.BigEndian.PutUint32(h[4:8], uint32(len(b)))
	prefixSum := sha256.Sum256(h[:8])
	copy(h[8:40], prefixSum[:])
	sum := sha256.Sum256(b)
	copy(h[40:], sum[:])
	return append(h, b...)
}
func rawAppend(t *testing.T, dir string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "journal.bin"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(b); err != nil {
		t.Fatal(err)
	}
	f.Close()
}
func hasCode(t *testing.T, err error, code core.ErrorCode) {
	t.Helper()
	var f *core.Fault
	if !errors.As(err, &f) || f.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestIdentityReceiptReplayAndExclusiveOwnership(t *testing.T) {
	j, dir := openTest(t)
	c := accepted(j, "a")
	appendOK(t, j, c)
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("second owner")
	}
	r, err := j.Recover(bg)
	if err != nil {
		t.Fatal(err)
	}
	r.Snapshot.Jobs[0].Request.Prompt = "mutated"
	r.Receipts[0].Key = "mutated"
	j.Close()
	j2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	r, err = j2.Recover(bg)
	if err != nil || r.Snapshot.Jobs[0].Request.Prompt != "a" || r.Receipts[0].Key != "a" || r.Snapshot.Cursor != c.Events[1].Cursor {
		t.Fatal(r, err)
	}
	events, err := j2.ReadEvents(bg, core.Cursor{StoreID: r.Snapshot.Cursor.StoreID}, 1)
	if err != nil || len(events) != 1 || events[0].Cursor.Sequence != 1 {
		t.Fatal(events, err)
	}
	events[0].Job.Request.Prompt = "changed"
	events, _ = j2.ReadEvents(bg, core.Cursor{StoreID: r.Snapshot.Cursor.StoreID}, 2)
	if events[0].Job.Request.Prompt != "a" {
		t.Fatal("mutable replay")
	}
	other, _ := Open(t.TempDir())
	defer other.Close()
	if other.state.Snapshot.Cursor.StoreID == r.Snapshot.Cursor.StoreID {
		t.Fatal("identity reuse")
	}
}

func TestTornFinalFrames(t *testing.T) {
	for _, kind := range []string{"header", "payload"} {
		t.Run(kind, func(t *testing.T) {
			j, dir := openTest(t)
			appendOK(t, j, accepted(j, "a"))
			size := j.size
			next := record(accepted(j, "b"))
			j.Close()
			if kind == "header" {
				next = next[:13]
			} else {
				next = next[:len(next)-7]
			}
			rawAppend(t, dir, next)
			recovered, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if recovered.RecoveredTailBytes != int64(len(next)) || recovered.size != size || len(recovered.state.Snapshot.Jobs) != 1 {
				t.Fatal("wrong torn-tail recovery")
			}
			appendOK(t, recovered, accepted(recovered, "b"))
		})
	}
}

func TestCorruptionFailsClosedPreservesEvidence(t *testing.T) {
	for _, kind := range []string{"checksum", "zero_length", "large_length", "frame_version", "json", "commit_version", "sequence", "offset", "queue", "receipt", "duplicate_field", "transition", "unknown_field", "header"} {
		t.Run(kind, func(t *testing.T) {
			j, dir := openTest(t)
			appendOK(t, j, accepted(j, "a"))
			c := accepted(j, "b")
			var tail []byte
			switch kind {
			case "checksum":
				tail = record(c)
				tail[len(tail)-1] ^= 1
			case "zero_length", "large_length", "frame_version":
				tail = record(c)
				if kind == "zero_length" {
					binary.BigEndian.PutUint32(tail[4:8], 0)
				} else if kind == "large_length" {
					binary.BigEndian.PutUint32(tail[4:8], core.MaxCommitBytes+1)
				} else {
					binary.BigEndian.PutUint32(tail[:4], 2)
				}
				sum := sha256.Sum256(tail[:8])
				copy(tail[8:40], sum[:])
			case "json":
				tail = frame([]byte("{"))
			case "commit_version":
				c.Version = 2
				tail = record(c)
			case "sequence":
				c.Events[0].Cursor.Sequence++
				tail = record(c)
			case "offset":
				c = transaction(j, []core.Event{{Kind: core.EventJobOutput, JobID: "a", Output: &core.OutputDelta{OffsetBytes: 99, Text: "x"}}}, nil)
				tail = record(c)
			case "queue":
				c.Events[1].Queue.Pending = nil
				tail = record(c)
			case "receipt":
				c.Receipt = nil
				tail = record(c)
			case "transition":
				c.Events[0].Job.State = core.JobSucceeded
				tail = record(c)
			case "duplicate_field", "unknown_field":
				b, _ := json.Marshal(c)
				prefix := `{"version":1,`
				if kind == "unknown_field" {
					prefix = `{"unknown":1,`
				}
				tail = frame(append([]byte(prefix), b[1:]...))
			}
			j.Close()
			path := filepath.Join(dir, "journal.bin")
			if kind == "header" {
				b, _ := os.ReadFile(path)
				b[8] = 99
				os.WriteFile(path, b, 0600)
			} else {
				rawAppend(t, dir, tail)
			}
			before, _ := os.ReadFile(path)
			if bad, err := Open(dir); err == nil {
				bad.Close()
				t.Fatal("corruption accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("evidence modified")
			}
		})
	}
}

type failingFile struct {
	diskFile
	writeMode string
	syncFail  bool
}

func (f *failingFile) Write(b []byte) (int, error) {
	switch f.writeMode {
	case "partial":
		n, _ := f.diskFile.Write(b[:len(b)/2])
		return n, io.ErrUnexpectedEOF
	case "zero":
		return 0, nil
	}
	return f.diskFile.Write(b)
}
func (f *failingFile) Sync() error {
	if f.syncFail {
		if err := f.diskFile.Sync(); err != nil {
			return err
		}
		return errors.New("injected ambiguous sync")
	}
	return f.diskFile.Sync()
}
func TestWriteSyncFailuresPoisonUntilRecovery(t *testing.T) {
	for _, mode := range []string{"partial", "zero", "sync"} {
		t.Run(mode, func(t *testing.T) {
			j, dir := openTest(t)
			before := j.state.Snapshot.Cursor
			c := accepted(j, "a")
			j.file = &failingFile{diskFile: j.file, writeMode: mode, syncFail: mode == "sync"}
			hasCode(t, j.Append(bg, c), core.ErrStorage)
			if j.state.Snapshot.Cursor != before {
				t.Fatal("acknowledged failed write")
			}
			hasCode(t, j.Append(bg, c), core.ErrStorage)
			_, err := j.Recover(bg)
			hasCode(t, err, core.ErrStorage)
			j.Close()
			recovered, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			want := 0
			if mode == "sync" {
				want = 1
			}
			if len(recovered.state.Snapshot.Jobs) != want || len(recovered.state.Receipts) != want {
				t.Fatal("ambiguous receipt recovery")
			}
		})
	}
}

func TestEncodedAndTotalStorageBounds(t *testing.T) {
	j, _ := openTest(t)
	c := accepted(j, "a")
	payload, _ := json.Marshal(c)
	next, _ := core.ApplyCommit(j.state.Snapshot, c)
	need := j.size + FrameSize + int64(len(payload)) + core.TerminalReserve(next)
	j.hard = need - 1
	hasCode(t, j.Append(bg, c), core.ErrLimitExceeded)
	if j.poisoned != nil || len(j.state.Snapshot.Jobs) != 0 {
		t.Fatal("preflight changed state")
	}
	j.hard = need
	j.admission = j.size + FrameSize + int64(len(payload)) - 1
	hasCode(t, j.Append(bg, c), core.ErrLimitExceeded)
	j.admission++
	appendOK(t, j, c)
	// A queued cancellation consumes its reserved terminal space even at the
	// exact admission-plus-reserve boundary.
	job := core.Clone(j.state.Snapshot.Jobs[0])
	job.State = core.JobCancelled
	now := time.Now().UTC()
	job.FinishedAt = &now
	job.Error = &core.Fault{Code: core.ErrCancelled, Message: "cancelled"}
	q := core.Clone(j.state.Snapshot.Queue)
	q.Pending = nil
	appendOK(t, j, transaction(j, []core.Event{{Kind: core.EventJobResult, JobID: job.ID, Job: &job}, {Kind: core.EventQueueState, Queue: &q}}, nil))
	c = accepted(j, "oversize")
	c.Events[0].Job.Origin = strings.Repeat("\x00", core.MaxCommitBytes/6+1)
	hasCode(t, j.Append(bg, c), core.ErrLimitExceeded)
}

func TestOutputExhaustionLeavesCancellationReserve(t *testing.T) {
	j, _ := openTest(t)
	makeRunning(t, j)
	j.hard = j.size + core.TerminalReserve(j.state.Snapshot)
	c := transaction(j, []core.Event{{Kind: core.EventJobOutput, JobID: "a", Output: &core.OutputDelta{OffsetBytes: 7, Text: "more"}}}, nil)
	hasCode(t, j.Append(bg, c), core.ErrLimitExceeded)
	job := core.Clone(j.state.Snapshot.Jobs[0])
	job.State = core.JobCancelling
	appendOK(t, j, transaction(j, []core.Event{{Kind: core.EventJobState, JobID: job.ID, Job: &job}}, nil))
	job.State = core.JobCancelled
	now := time.Now().UTC()
	job.FinishedAt = &now
	job.Error = &core.Fault{Code: core.ErrCancelled, Message: "cancelled"}
	q := core.Clone(j.state.Snapshot.Queue)
	q.Active = nil
	appendOK(t, j, transaction(j, []core.Event{{Kind: core.EventJobResult, JobID: job.ID, Job: &job}, {Kind: core.EventQueueState, Queue: &q}}, nil))
}

func TestReadCorruptionPoisonsHandle(t *testing.T) {
	j, dir := openTest(t)
	appendOK(t, j, accepted(j, "a"))
	f, err := os.OpenFile(filepath.Join(dir, "journal.bin"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{0}, HeaderSize+FrameSize); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, err = j.ReadEvents(bg, core.Cursor{StoreID: j.state.Snapshot.Cursor.StoreID}, 1)
	hasCode(t, err, core.ErrStorage)
	hasCode(t, j.Append(bg, accepted(j, "b")), core.ErrStorage)
}

type unusedBackend struct{}

func (unusedBackend) Models(context.Context) ([]core.Model, error) { return nil, nil }
func (unusedBackend) Generate(context.Context, core.InferenceInput, func(string) error) (core.Result, error) {
	panic("unexpected inference")
}
func makeRunning(t *testing.T, j *Journal) {
	appendOK(t, j, accepted(j, "a"))
	job := core.Clone(j.state.Snapshot.Jobs[0])
	job.State = core.JobRunning
	now := time.Now().UTC()
	job.StartedAt = &now
	q := core.Clone(j.state.Snapshot.Queue)
	q.Pending = nil
	q.Active = &job.ID
	appendOK(t, j, transaction(j, []core.Event{{Kind: core.EventJobState, JobID: job.ID, Job: &job}, {Kind: core.EventQueueState, Queue: &q}}, nil))
	appendOK(t, j, transaction(j, []core.Event{{Kind: core.EventJobOutput, JobID: job.ID, Output: &core.OutputDelta{Text: "partial"}}}, nil))
}

func TestRecoveryInterruptsBeforeDispatch(t *testing.T) {
	for _, cancelling := range []bool{false, true} {
		t.Run(fmtBool(cancelling), func(t *testing.T) {
			j, dir := openTest(t)
			makeRunning(t, j)
			if cancelling {
				job := core.Clone(j.state.Snapshot.Jobs[0])
				job.State = core.JobCancelling
				appendOK(t, j, transaction(j, []core.Event{{Kind: core.EventJobState, JobID: job.ID, Job: &job}}, nil))
			}
			previous := j.state.Snapshot.Cursor.Sequence
			j.Close()
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			w, err := core.NewWorker(reopened, unusedBackend{})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Shutdown(bg)
			s, _ := w.Snapshot(bg)
			if s.Jobs[0].State != core.JobInterrupted || s.Jobs[0].Result.Text != "partial" || s.Cursor.Sequence != previous+2 || s.Queue.Active != nil {
				t.Fatal(s)
			}
		})
	}
}
func fmtBool(v bool) string {
	if v {
		return "cancelling"
	}
	return "running"
}

func TestProcessDeathReleasesOwnership(t *testing.T) {
	if dir := os.Getenv("TASKWORKER_TEST_CRASH_DIR"); dir != "" {
		j, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		makeRunning(t, j)
		os.Stdout.WriteString("READY\n")
		var b [1]byte
		os.Stdin.Read(b[:])
		os.Exit(4)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessDeathReleasesOwnership$")
	cmd.Env = append(os.Environ(), "TASKWORKER_TEST_CRASH_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		t.Fatal("child did not acquire lock")
	}
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("child lock not exclusive")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if j.state.Snapshot.Jobs[0].Result.Text != "partial" {
		t.Fatal("lost synced child output")
	}
}
