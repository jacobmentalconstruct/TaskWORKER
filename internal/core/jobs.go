package core

import "time"

type JobID string

type JobState string

const (
	JobQueued      JobState = "queued"
	JobRunning     JobState = "running"
	JobCancelling  JobState = "cancelling"
	JobSucceeded   JobState = "succeeded"
	JobFailed      JobState = "failed"
	JobCancelled   JobState = "cancelled"
	JobInterrupted JobState = "interrupted"
)

// Request is immutable after acceptance. Role is an instruction/persona, not a
// chat protocol role. Nil option values mean unspecified, never zero.
type Request struct {
	Model        string            `json:"model"`
	Role         string            `json:"role,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Prompt       string            `json:"prompt"`
	Options      GenerationOptions `json:"options"`
}

type GenerationOptions struct {
	ContextTokens   *int     `json:"context_tokens,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	Temperature     *float64 `json:"temperature,omitempty"`
	Seed            *int64   `json:"seed,omitempty"`
}

// Instructions is the canonical, recorded backend input. History is empty for
// fresh jobs. It is materialized only by explicit branching or retry of a branch.
type Instructions struct {
	System  string    `json:"system"`
	History []Message `json:"history,omitempty"`
	Prompt  string    `json:"prompt"`
}

type MessageRole string

const (
	MessageUser      MessageRole = "user"
	MessageAssistant MessageRole = "assistant"
)

type Message struct {
	Role    MessageRole `json:"role"`
	Content string      `json:"content"`
}

type Relation string

const (
	RelationRetry  Relation = "retry"
	RelationBranch Relation = "branch"
)

type Lineage struct {
	ParentID JobID    `json:"parent_id"`
	Relation Relation `json:"relation"`
}

type Job struct {
	ID           JobID        `json:"id"`
	State        JobState     `json:"state"`
	Request      Request      `json:"request"`
	Instructions Instructions `json:"instructions"`
	Origin       string       `json:"origin,omitempty"` // display label, not authentication
	Lineage      *Lineage     `json:"lineage,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	StartedAt    *time.Time   `json:"started_at,omitempty"`
	FinishedAt   *time.Time   `json:"finished_at,omitempty"`
	Result       Result       `json:"result"` // retained partial text in every state
	Error        *Fault       `json:"error,omitempty"`
}

type Result struct {
	Text             string             `json:"text"`
	FinishReason     string             `json:"finish_reason,omitempty"`
	EffectiveModel   string             `json:"effective_model,omitempty"`
	EffectiveOptions *GenerationOptions `json:"effective_options,omitempty"`
	Context          ContextInfo        `json:"context"`
	Usage            Usage              `json:"usage"`
}

// Nil counts and durations are unknown, not zero. Durations use nanoseconds.
type Usage struct {
	InputTokens        *int   `json:"input_tokens,omitempty"`
	OutputTokens       *int   `json:"output_tokens,omitempty"`
	TotalDurationNS    *int64 `json:"total_duration_ns,omitempty"`
	LoadDurationNS     *int64 `json:"load_duration_ns,omitempty"`
	GenerateDurationNS *int64 `json:"generate_duration_ns,omitempty"`
}

type TokenEstimate struct {
	Tokens int    `json:"tokens"`
	Method string `json:"method"` // e.g. exact tokenizer or conservative character estimate
	Exact  bool   `json:"exact"`
}

type ContextObservation struct {
	Tokens     int       `json:"tokens"`
	Model      string    `json:"model"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}

// Capacity, request, and loaded observation must never be substituted for one
// another. A loaded observation is a timestamped backend report, not a guarantee.
type ContextInfo struct {
	ModelCapacityTokens     *int                `json:"model_capacity_tokens,omitempty"`
	ModelCapacitySource     string              `json:"model_capacity_source,omitempty"`
	ModelCapacityObservedAt *time.Time          `json:"model_capacity_observed_at,omitempty"`
	RequestedTokens         *int                `json:"requested_tokens,omitempty"`
	Loaded                  *ContextObservation `json:"loaded,omitempty"`
	InputEstimate           *TokenEstimate      `json:"input_estimate,omitempty"`
	ReservedOutputTokens    int                 `json:"reserved_output_tokens"`
}
