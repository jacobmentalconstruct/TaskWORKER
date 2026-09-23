package core

import (
	"context"
	"io"
	"sync"
)

type queuedEvent struct {
	event Event
	size  int
}
type stream struct {
	owner   *Worker
	mu      sync.Mutex
	nextMu  sync.Mutex
	cursor  Cursor
	through uint64
	queue   []queuedEvent
	bytes   int
	wake    chan struct{}
	closed  bool
	err     error
	detach  func() bool
}

func (w *Worker) Watch(ctx context.Context, c Cursor) (EventStream, error) {
	return call(ctx, func() (EventStream, error) { return w.watch(ctx, c) })
}
func (w *Worker) watch(ctx context.Context, c Cursor) (EventStream, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.available(); err != nil {
		return nil, err
	}
	if err := ValidateCursor(c, w.state.Cursor); err != nil {
		return nil, err
	}
	s := &stream{owner: w, cursor: c, through: w.state.Cursor.Sequence, wake: make(chan struct{}, 1)}
	w.observers[s] = true
	s.detach = context.AfterFunc(ctx, func() { s.Close() })
	return s, nil
}
func (s *stream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *stream) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.err = err
	s.queue = nil
	s.bytes = 0
	s.signal()
}
func (s *stream) push(e Event, size int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if len(s.queue) >= MaxObserverEvents || size > MaxObserverBytes-s.bytes {
		s.closed = true
		s.err = fault(ErrSlowConsumer, "observer backlog exceeded; reconnect from last applied cursor")
		s.queue = nil
		s.bytes = 0
		s.signal()
		return false
	}
	s.queue = append(s.queue, queuedEvent{Clone(e), size})
	s.bytes += size
	s.signal()
	return true
}
func (s *stream) Next(ctx context.Context) (Event, error) {
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		s.mu.Lock()
		if s.closed {
			err := s.err
			s.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return Event{}, err
		}
		if s.cursor.Sequence < s.through {
			after := s.cursor
			s.mu.Unlock()
			events, err := call(ctx, func() ([]Event, error) { return s.owner.store.ReadEvents(ctx, after, 1) })
			if err != nil {
				// Read-side storage failures are as authoritative as write-side
				// failures; do not continue inference against suspect history.
				if ctx.Err() == nil {
					s.owner.mu.Lock()
					if !s.owner.stopping {
						s.owner.poison(err)
					}
					s.owner.mu.Unlock()
				}
				return Event{}, err
			}
			if len(events) != 1 || events[0].Cursor.Sequence > s.through {
				err := fault(ErrStorage, "replay gap")
				s.owner.mu.Lock()
				if !s.owner.stopping {
					s.owner.poison(err)
				}
				s.owner.mu.Unlock()
				return Event{}, err
			}
			s.mu.Lock()
			s.cursor = events[0].Cursor
			s.mu.Unlock()
			return events[0], nil
		}
		if len(s.queue) > 0 {
			q := s.queue[0]
			s.queue[0] = queuedEvent{}
			s.queue = s.queue[1:]
			s.bytes -= q.size
			s.cursor = q.event.Cursor
			s.mu.Unlock()
			return q.event, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-s.wake:
		}
	}
}
func (s *stream) Close() error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	delete(s.owner.observers, s)
	s.finish(nil)
	if s.detach != nil {
		s.detach()
	}
	return nil
}
