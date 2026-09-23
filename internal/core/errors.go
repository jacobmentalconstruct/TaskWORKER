package core

type ErrorCode string

const (
	ErrInvalidRequest     ErrorCode = "invalid_request"
	ErrNotFound           ErrorCode = "not_found"
	ErrConflict           ErrorCode = "conflict"
	ErrQueueFull          ErrorCode = "queue_full"
	ErrLimitExceeded      ErrorCode = "limit_exceeded"
	ErrUnsupported        ErrorCode = "unsupported"
	ErrBackendUnavailable ErrorCode = "backend_unavailable"
	ErrBackendFailure     ErrorCode = "backend_failure"
	ErrCancelled          ErrorCode = "cancelled"
	ErrInterrupted        ErrorCode = "interrupted"
	ErrCursorExpired      ErrorCode = "cursor_expired"
	ErrCursorInvalid      ErrorCode = "cursor_invalid"
	ErrSlowConsumer       ErrorCode = "slow_consumer"
	ErrStorage            ErrorCode = "storage_failure"
	ErrUnavailable        ErrorCode = "unavailable"
	ErrInternal           ErrorCode = "internal"
)

// Fault is the stable error body across adapters. Retryable is advice to callers;
// it never requests an automatic inference retry. Details must not leak prompts.
type Fault struct {
	Code      ErrorCode         `json:"code"`
	Message   string            `json:"message"`
	Retryable bool              `json:"retryable"`
	Details   map[string]string `json:"details,omitempty"`
}

func (f *Fault) Error() string { return string(f.Code) + ": " + f.Message }
