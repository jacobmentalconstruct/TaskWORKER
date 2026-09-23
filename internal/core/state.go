package core

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	MaxPending        = 64
	MaxJobs           = 1000
	MaxInputBytes     = 1 << 20
	MaxOutputBytes    = 8 << 20
	MaxCommitBytes    = 16 << 20
	MaxChunkBytes     = 16 << 10
	MaxObserverEvents = 256
	MaxObserverBytes  = 32 << 20
	MetadataReserve   = 64 << 10
)

func fault(code ErrorCode, message string) *Fault { return &Fault{Code: code, Message: message} }

// Clone returns a deep copy of a JSON-compatible contract value.
func Clone[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func Terminal(s JobState) bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled || s == JobInterrupted
}

func ValidateRequest(r Request, in Instructions) error {
	if strings.TrimSpace(r.Model) == "" || strings.TrimSpace(r.Prompt) == "" {
		return fault(ErrInvalidRequest, "model and prompt are required")
	}
	for _, s := range []string{r.Model, r.Role, r.SystemPrompt, r.Prompt, in.System, in.Prompt} {
		if !utf8.ValidString(s) {
			return fault(ErrInvalidRequest, "input must be UTF-8")
		}
	}
	o := r.Options
	if o.MaxOutputTokens <= 0 || (o.ContextTokens != nil && (*o.ContextTokens <= 0 || *o.ContextTokens <= o.MaxOutputTokens)) || (o.Temperature != nil && (math.IsNaN(*o.Temperature) || math.IsInf(*o.Temperature, 0) || *o.Temperature < 0)) {
		return fault(ErrInvalidRequest, "invalid generation budget or options")
	}
	n := len(in.System) + len(in.Prompt)
	for _, m := range in.History {
		if (m.Role != MessageUser && m.Role != MessageAssistant) || !utf8.ValidString(m.Content) {
			return fault(ErrInvalidRequest, "invalid history")
		}
		n += len(m.Content)
	}
	if n > MaxInputBytes || len(r.Model)+len(r.Role)+len(r.SystemPrompt)+len(r.Prompt) > MaxInputBytes {
		return fault(ErrLimitExceeded, "input limit exceeded")
	}
	return nil
}

func Compose(r Request) Instructions {
	parts := []string{}
	for _, s := range []string{r.Role, r.SystemPrompt} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return Instructions{System: strings.Join(parts, "\n\n"), Prompt: r.Prompt}
}

func transition(a, b JobState) bool {
	switch a {
	case JobQueued:
		return b == JobRunning || b == JobCancelled
	case JobRunning:
		return b == JobCancelling || b == JobSucceeded || b == JobFailed || b == JobInterrupted
	case JobCancelling:
		return b == JobCancelled || b == JobInterrupted
	}
	return false
}

// ApplyCommit validates a transaction without modifying its input. Both the
// live owner and recovery use this reducer; queue invariants hold at commit edges.
func ApplyCommit(s Snapshot, c Commit) (Snapshot, error) {
	bad := func() (Snapshot, error) { return Snapshot{}, fault(ErrStorage, "invalid journal transaction") }
	if c.Version != 1 || len(c.Events) == 0 {
		return bad()
	}
	// Copy the job slice; existing nested values are immutable and replaced,
	// never edited by the reducer. Avoid re-encoding all retained output per delta.
	s.Jobs = append([]Job(nil), s.Jobs...)
	s.Queue = Clone(s.Queue)
	accepted := JobID("")
	find := func(id JobID) int {
		for i := range s.Jobs {
			if s.Jobs[i].ID == id {
				return i
			}
		}
		return -1
	}
	for _, e := range c.Events {
		if s.Cursor.Sequence == math.MaxUint64 || e.Cursor.StoreID != s.Cursor.StoreID || e.Cursor.Sequence != s.Cursor.Sequence+1 || e.At.IsZero() {
			return bad()
		}
		count := 0
		if e.Job != nil {
			count++
		}
		if e.Output != nil {
			count++
		}
		if e.Queue != nil {
			count++
		}
		if count != 1 {
			return bad()
		}
		i := find(e.JobID)
		switch e.Kind {
		case EventJobAccepted, EventJobState, EventJobResult:
			if e.Job == nil || e.Job.ID == "" || e.JobID != e.Job.ID {
				return bad()
			}
			j := Clone(*e.Job)
			if ValidateRequest(j.Request, j.Instructions) != nil || j.CreatedAt.IsZero() || !utf8.ValidString(j.Result.Text) || len(j.Result.Text) > MaxOutputBytes {
				return bad()
			}
			if !utf8.ValidString(j.Origin) || len(j.Origin) > 1024 {
				return bad()
			}
			if e.Kind == EventJobAccepted {
				if CheckJobFits(j) != nil {
					return bad()
				}
				if i >= 0 || accepted != "" || len(s.Jobs) >= MaxJobs || j.State != JobQueued || j.StartedAt != nil || j.FinishedAt != nil || !reflect.DeepEqual(j.Result, Result{}) || j.Error != nil {
					return bad()
				}
				expected := Compose(j.Request)
				if j.Lineage != nil {
					p := find(j.Lineage.ParentID)
					if p < 0 || !Terminal(s.Jobs[p].State) {
						return bad()
					}
					parent := s.Jobs[p]
					switch j.Lineage.Relation {
					case RelationRetry:
						if !reflect.DeepEqual(j.Request, parent.Request) {
							return bad()
						}
						expected = parent.Instructions
					case RelationBranch:
						expected.History = append(expected.History, parent.Instructions.History...)
						expected.History = append(expected.History, Message{Role: MessageUser, Content: parent.Instructions.Prompt})
						if parent.Result.Text != "" {
							expected.History = append(expected.History, Message{Role: MessageAssistant, Content: parent.Result.Text})
						}
					default:
						return bad()
					}
				}
				if !reflect.DeepEqual(expected, j.Instructions) {
					return bad()
				}
				accepted = j.ID
				s.Jobs = append(s.Jobs, j)
			} else {
				if i < 0 || !transition(s.Jobs[i].State, j.State) {
					return bad()
				}
				old := s.Jobs[i]
				if j.State == JobRunning && (s.Queue.Paused || s.Queue.Active != nil || len(s.Queue.Pending) == 0 || s.Queue.Pending[0] != j.ID) {
					return bad()
				}
				if !reflect.DeepEqual(old.Request, j.Request) || !reflect.DeepEqual(old.Instructions, j.Instructions) || !reflect.DeepEqual(old.Lineage, j.Lineage) || old.Origin != j.Origin || !old.CreatedAt.Equal(j.CreatedAt) || old.Result.Text != j.Result.Text {
					return bad()
				}
				if (Terminal(j.State) != (e.Kind == EventJobResult)) || (Terminal(j.State) != (j.FinishedAt != nil)) || ((j.State == JobRunning || j.State == JobCancelling || old.StartedAt != nil) && j.StartedAt == nil) {
					return bad()
				}
				if old.StartedAt != nil && !reflect.DeepEqual(old.StartedAt, j.StartedAt) {
					return bad()
				}
				if !Terminal(j.State) && (!reflect.DeepEqual(old.Result, j.Result) || j.Error != nil) {
					return bad()
				}
				if j.State == JobSucceeded && j.Error != nil {
					return bad()
				}
				if Terminal(j.State) && j.State != JobSucceeded && (j.Error == nil || j.Error.Code == "") {
					return bad()
				}
				s.Jobs[i] = j
			}
		case EventJobOutput:
			if e.Output == nil || i < 0 || (s.Jobs[i].State != JobRunning && s.Jobs[i].State != JobCancelling) || e.Output.OffsetBytes != int64(len(s.Jobs[i].Result.Text)) || len(e.Output.Text) == 0 || len(e.Output.Text) > MaxChunkBytes || !utf8.ValidString(e.Output.Text) || len(s.Jobs[i].Result.Text)+len(e.Output.Text) > MaxOutputBytes {
				return bad()
			}
			s.Jobs[i].Result.Text += e.Output.Text
			if CheckJobFits(s.Jobs[i]) != nil {
				return bad()
			}
		case EventQueueState:
			if e.Queue == nil || e.JobID != "" || e.Queue.MaxPending != MaxPending {
				return bad()
			}
			s.Queue = Clone(*e.Queue)
		default:
			return bad()
		}
		s.Cursor = e.Cursor
	}
	if (accepted != "") != (c.Receipt != nil) {
		return bad()
	}
	if c.Receipt != nil {
		r := c.Receipt
		if r.JobID != accepted || len(r.Key) == 0 || len(r.Key) > 128 || !utf8.ValidString(r.Key) || len(r.Digest) != 64 {
			return bad()
		}
		for _, x := range r.Digest {
			if !strings.ContainsRune("0123456789abcdef", x) {
				return bad()
			}
		}
	}
	pending := []JobID{}
	active := JobID("")
	for _, j := range s.Jobs {
		if j.State == JobQueued {
			pending = append(pending, j.ID)
		}
		if j.State == JobRunning || j.State == JobCancelling {
			if active != "" {
				return bad()
			}
			active = j.ID
		}
	}
	if len(pending) > MaxPending || len(pending) != len(s.Queue.Pending) {
		return bad()
	}
	for i := range pending {
		if pending[i] != s.Queue.Pending[i] {
			return bad()
		}
	}
	if (active == "") != (s.Queue.Active == nil) || (s.Queue.Active != nil && *s.Queue.Active != active) {
		return bad()
	}
	return s, nil
}

// TerminalReserve includes encoded replacement events for every unfinished job,
// framing/queue overhead, and bounded backend metadata. It is recomputed on append.
func TerminalReserve(s Snapshot) int64 {
	n := int64(8192)
	for _, j := range s.Jobs {
		if !Terminal(j.State) {
			b, _ := json.Marshal(j)
			// Queued work can still need running, cancelling, and terminal
			// replacements. Cancellation must fit after output has filled storage.
			replacements := 1
			if j.State == JobRunning {
				replacements = 2
			}
			if j.State == JobQueued {
				replacements = 3
			}
			n += int64(replacements*(len(b)+4096) + MetadataReserve)
		}
	}
	return n
}

func CheckJobFits(j Job) error {
	b, err := json.Marshal(j)
	if err != nil || len(b)+MetadataReserve+8192 > MaxCommitBytes {
		return fault(ErrLimitExceeded, "encoded job exceeds terminal record budget")
	}
	return nil
}

func ValidateCursor(c, end Cursor) error {
	if c.StoreID != end.StoreID || c.Sequence > end.Sequence {
		return fault(ErrCursorInvalid, "cursor does not belong to available history")
	}
	return nil
}

// UnmarshalJSON rejects noncanonical decimal cursors, including leading zeroes.
func (c *Cursor) UnmarshalJSON(b []byte) error {
	type wire struct {
		StoreID  string `json:"store_id"`
		Sequence string `json:"sequence"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if w.Sequence == "" || (len(w.Sequence) > 1 && w.Sequence[0] == '0') {
		return fmt.Errorf("invalid cursor sequence")
	}
	var n uint64
	for _, r := range w.Sequence {
		if r < '0' || r > '9' || n > (math.MaxUint64-uint64(r-'0'))/10 {
			return fmt.Errorf("invalid cursor sequence")
		}
		n = n*10 + uint64(r-'0')
	}
	c.StoreID = w.StoreID
	c.Sequence = n
	return nil
}
