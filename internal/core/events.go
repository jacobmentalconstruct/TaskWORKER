package core

import "time"

// Cursor identifies a committed event in one persistent store. Sequence is a
// decimal JSON string to preserve all uint64 values in browser clients.
type Cursor struct {
	StoreID  string `json:"store_id"`
	Sequence uint64 `json:"sequence,string"`
}

type EventKind string

const (
	EventJobAccepted EventKind = "job.accepted"
	EventJobState    EventKind = "job.state"
	EventJobOutput   EventKind = "job.output"
	EventJobResult   EventKind = "job.result"
	EventQueueState  EventKind = "queue.state"
)

// Event has exactly the payload named by Kind. Job is a complete replacement
// for accepted/state/result events; Output appends text; Queue replaces queue
// state. Output offsets count UTF-8 bytes and reject duplicate application.
type Event struct {
	Cursor Cursor       `json:"cursor"`
	At     time.Time    `json:"at"`
	Kind   EventKind    `json:"kind"`
	JobID  JobID        `json:"job_id,omitempty"`
	Job    *Job         `json:"job,omitempty"`
	Output *OutputDelta `json:"output,omitempty"`
	Queue  *QueueState  `json:"queue,omitempty"`
}

type OutputDelta struct {
	OffsetBytes int64  `json:"offset_bytes"`
	Text        string `json:"text"`
}

type QueueState struct {
	Paused     bool    `json:"paused"`
	Pending    []JobID `json:"pending"` // FIFO acceptance order
	Active     *JobID  `json:"active,omitempty"`
	MaxPending int     `json:"max_pending"`
}

// Snapshot is an atomic view through Cursor. All retained jobs are included.
// Watch starts strictly AFTER this cursor and must bridge replay to live delivery.
type Snapshot struct {
	Cursor Cursor     `json:"cursor"`
	Queue  QueueState `json:"queue"`
	Jobs   []Job      `json:"jobs"`
}
