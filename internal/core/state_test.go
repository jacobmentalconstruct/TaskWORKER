package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestCursorCanonicalDecimal(t *testing.T) {
	for _, value := range []string{`""`, `"01"`, `"-1"`, `"+1"`, `"1.0"`, `"18446744073709551616"`, `1`, `null`} {
		var c Cursor
		if json.Unmarshal([]byte(`{"store_id":"a","sequence":`+value+`}`), &c) == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	for _, value := range []string{"0", "1", "18446744073709551615"} {
		var c Cursor
		if err := json.Unmarshal([]byte(`{"store_id":"a","sequence":"`+value+`"}`), &c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRequestAndEncodedBounds(t *testing.T) {
	r := Request{Model: "fake", Prompt: "prompt", Options: GenerationOptions{MaxOutputTokens: 1}}
	for _, temp := range []float64{math.NaN(), math.Inf(1), -1} {
		r.Options.Temperature = &temp
		if ValidateRequest(r, Compose(r)) == nil {
			t.Fatal("invalid temperature")
		}
	}
	r.Options.Temperature = nil
	zero := 0
	r.Options.ContextTokens = &zero
	if ValidateRequest(r, Compose(r)) == nil {
		t.Fatal("zero context")
	}
	r.Options.ContextTokens = nil
	in := Compose(r)
	in.History = []Message{{Role: MessageUser, Content: strings.Repeat("a", MaxInputBytes)}}
	if ValidateRequest(r, in) == nil {
		t.Fatal("history excluded from input bound")
	}
	j := Job{Request: r, Instructions: Compose(r), Result: Result{Text: strings.Repeat("\x00", MaxCommitBytes/6)}}
	if CheckJobFits(j) == nil {
		t.Fatal("JSON expansion unaccounted")
	}
}

func TestObserverByteLimitAndOwnership(t *testing.T) {
	s := &stream{wake: make(chan struct{}, 1)}
	q := QueueState{Pending: []JobID{"a"}}
	e := Event{Kind: EventQueueState, Queue: &q}
	if !s.push(e, MaxObserverBytes) {
		t.Fatal("exact bound rejected")
	}
	q.Pending[0] = "mutated"
	if s.queue[0].event.Queue.Pending[0] != "a" {
		t.Fatal("event ownership")
	}
	if s.push(e, 1) {
		t.Fatal("byte bound ignored")
	}
	if s.bytes != 0 || len(s.queue) != 0 {
		t.Fatal("backlog retained after disconnect")
	}
	_, err := s.Next(context.Background())
	var f *Fault
	if !errors.As(err, &f) || f.Code != ErrSlowConsumer {
		t.Fatal(err)
	}
}
