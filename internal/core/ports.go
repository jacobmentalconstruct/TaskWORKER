package core

import "context"

type Capabilities struct {
	TextGeneration   bool `json:"text_generation"`
	Streaming        bool `json:"streaming"`
	Cancellation     bool `json:"cancellation"`
	ContextRequest   bool `json:"context_request"`
	LoadedContext    bool `json:"loaded_context"`
	ExactPauseResume bool `json:"exact_pause_resume"`
	Checkpoints      bool `json:"checkpoints"`
}

type Model struct {
	ID           string       `json:"id"`
	Capabilities Capabilities `json:"capabilities"`
	Context      ContextInfo  `json:"context"`
}

type InferenceInput struct {
	Request      Request      `json:"request"`
	Instructions Instructions `json:"instructions"`
}

// Backend has no queue or persisted state. Generate calls emit serially, stops
// on cancellation or emit failure, and returns only after callbacks have stopped.
// The core owns accumulated text; the returned Result supplies final metadata.
// Generate must validate selected model capabilities, supported options, and
// known context budgets before generation; refresh backend-specific observations.
// Unknown quantities stay unknown. Emit accepts valid UTF-8, which core splits
// into durable chunks. Returned metadata must encode to at most 32 KiB without
// Text; effective settings must be observed, never invented from the request.
// Generate and Models errors use the backend fault allowlist in docs/contracts.md.
// Only backend_unavailable/backend_failure may retain typed retry advice; core
// replaces all backend error messages and discards details before publication.
type Backend interface {
	Models(context.Context) ([]Model, error)
	Generate(context.Context, InferenceInput, func(text string) error) (Result, error)
}

type SubmitCommand struct {
	Key     string  `json:"idempotency_key"`
	Origin  string  `json:"origin,omitempty"`
	Request Request `json:"request"`
}

type RetryCommand struct {
	Key      string `json:"idempotency_key"`
	Origin   string `json:"origin,omitempty"`
	ParentID JobID  `json:"parent_id"`
}

type BranchCommand struct {
	Key      string  `json:"idempotency_key"`
	Origin   string  `json:"origin,omitempty"`
	ParentID JobID   `json:"parent_id"`
	Request  Request `json:"request"` // complete new settings, no implicit override merge
}

// Service is the shared use-case boundary. The authoritative implementation
// belongs to serve; network clients may implement this interface as proxies.
// Call contexts limit waiting, never the lifetime of an accepted job.
type Service interface {
	Submit(context.Context, SubmitCommand) (Job, error)
	Retry(context.Context, RetryCommand) (Job, error)
	Branch(context.Context, BranchCommand) (Job, error)
	Cancel(context.Context, JobID) (Job, error)
	SetQueuePaused(context.Context, bool) (QueueState, error)
	GetJob(context.Context, JobID) (Job, error)
	Snapshot(context.Context) (Snapshot, error)
	Watch(context.Context, Cursor) (EventStream, error)
	Models(context.Context) ([]Model, error)
}

// EventStream is bounded. Next returns io.EOF on orderly close, or a Fault for
// slow consumers / invalid cursors. Close releases resources; it cannot cancel jobs.
type EventStream interface {
	Next(context.Context) (Event, error)
	Close() error
}

// Receipt makes accepted create commands deduplicate across disconnect/restart.
// Digest covers operation, parent, origin, and canonical typed request fields.
type Receipt struct {
	Key    string `json:"key"`
	Digest string `json:"digest"`
	JobID  JobID  `json:"job_id"`
}

// Commit is one atomic durable journal record. Events are the authoritative
// state mutations; a create receipt must commit with its accepted event.
type Commit struct {
	Version int      `json:"version"`
	Events  []Event  `json:"events"`
	Receipt *Receipt `json:"receipt,omitempty"`
}

type Recovery struct {
	Snapshot Snapshot  `json:"snapshot"`
	Receipts []Receipt `json:"receipts"`
}

// Store has one writer exclusively owned by serve. Open/locking are adapter
// concerns. Append validates contiguous cursors and returns only after sync.
// Any ambiguous write error poisons the store until it is reopened/recovered.
// ErrLimitExceeded is a definite pre-write rejection and does not poison a store.
// Recover/ReadEvents return owned copies. ReadEvents supports pages of 1..256;
// implementations may return fewer events to maintain a bounded byte page.
type Store interface {
	Recover(context.Context) (Recovery, error)
	Append(context.Context, Commit) error
	ReadEvents(ctx context.Context, after Cursor, limit int) ([]Event, error)
	Close() error
}
