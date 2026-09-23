package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/core"
)

const modelID = "tiny:latest"
const partial = `{"model":"tiny:latest","message":{"role":"assistant","content":"partial","thinking":"hidden","tool_calls":[{"function":{"name":"do_not_execute"}}]},"done":false}` + "\n"
const terminal = `{"model":"tiny:latest","message":{"role":"assistant","content":"!"},"done":true,"done_reason":"stop","prompt_eval_count":20,"eval_count":2,"total_duration":100,"load_duration":10,"eval_duration":80}` + "\n"

type fixture struct {
	version  string
	tags     any
	show     any
	ps       any
	chat     func(http.ResponseWriter, *http.Request, chatRequest)
	hook     func(http.ResponseWriter, *http.Request) bool
	mu       sync.Mutex
	requests []chatRequest
	shows    atomic.Int32
}

func defaults() *fixture {
	return &fixture{version: supportedVersion,
		tags: map[string]any{"models": []any{map[string]any{"name": modelID, "digest": "digest"}}},
		show: map[string]any{"details": map[string]string{"format": "gguf"}, "capabilities": []string{"completion"}, "template": "{{ .Messages }}", "modelfile": "FROM /local/weights\nTEMPLATE {{ .Messages }}\n", "model_info": map[string]any{"general.architecture": "test", "test.context_length": 8192}},
		ps:   map[string]any{"models": []any{map[string]any{"name": modelID, "digest": "digest", "context_length": 4096}}}}
}

func (f *fixture) server(t *testing.T) (*Backend, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.hook != nil && f.hook(w, r) {
			return
		}
		switch r.URL.Path {
		case "/api/version":
			json.NewEncoder(w).Encode(map[string]string{"version": f.version})
		case "/api/tags":
			json.NewEncoder(w).Encode(f.tags)
		case "/api/show":
			f.shows.Add(1)
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			if v["model"] != modelID+":local" {
				t.Errorf("show reference: %v", v)
			}
			json.NewEncoder(w).Encode(f.show)
		case "/api/ps":
			json.NewEncoder(w).Encode(f.ps)
		case "/api/chat":
			var v chatRequest
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			f.mu.Lock()
			f.requests = append(f.requests, v)
			f.mu.Unlock()
			if len(v.Messages) == 0 {
				io.WriteString(w, `{"model":"tiny:latest:local","message":{"role":"assistant"},"done":true,"done_reason":"load"}`)
				return
			}
			if f.chat != nil {
				f.chat(w, r, v)
				return
			}
			io.WriteString(w, partial+terminal)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	// The operation deadline is generous: under the race detector the 32 MiB wire-limit test takes about
	// 3s on a loaded runner, and a deadline that expires first is reported as backend_unavailable.
	// Tests that exercise the deadline itself set their own short value.
	b, err := New(Config{Endpoint: s.URL, Client: s.Client(), Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.CloseIdleConnections(); s.Close() })
	return b, s
}

func input() core.InferenceInput {
	n := 4096
	r := core.Request{Model: modelID, Role: " role ", SystemPrompt: " system ", Prompt: " user prompt ", Options: core.GenerationOptions{ContextTokens: &n, MaxOutputTokens: 64}}
	return core.InferenceInput{Request: r, Instructions: core.Compose(r)}
}

func requireCode(t *testing.T, err error, code core.ErrorCode) {
	t.Helper()
	var f *core.Fault
	if !errors.As(err, &f) || f.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if f.Retryable != (code == core.ErrBackendUnavailable) || f.Details != nil || len(f.Message) > 128 || strings.Contains(f.Message, "SECRET") {
		t.Fatalf("unsafe fault: %+v", f)
	}
}

func TestDiscoveryAndTranslation(t *testing.T) {
	f := defaults()
	b, _ := f.server(t)
	models, err := b.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || !models[0].Capabilities.TextGeneration || models[0].Capabilities.Checkpoints || models[0].Capabilities.ExactPauseResume || *models[0].Context.ModelCapacityTokens != 8192 || models[0].Context.RequestedTokens != nil || models[0].Context.Loaded.Tokens != 4096 || models[0].Context.ModelCapacityObservedAt.IsZero() {
		t.Fatalf("metadata: %+v", models)
	}
	in := input()
	temp := 0.2
	seed := int64(42)
	in.Request.Options.Temperature = &temp
	in.Request.Options.Seed = &seed
	in.Instructions.History = []core.Message{{Role: core.MessageUser, Content: "old prompt"}, {Role: core.MessageAssistant, Content: "retained partial"}}
	var text strings.Builder
	r, err := b.Generate(context.Background(), in, func(s string) error { text.WriteString(s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "partial!" || r.Text != "" || r.EffectiveOptions != nil || r.FinishReason != "stop" || *r.Usage.InputTokens != 20 || *r.Usage.OutputTokens != 2 || *r.Usage.TotalDurationNS != 100 || *r.Usage.LoadDurationNS != 10 || *r.Usage.GenerateDurationNS != 80 {
		t.Fatalf("result: %+v / %s", r, text.String())
	}
	if r.Context.InputEstimate.Exact || !strings.Contains(r.Context.InputEstimate.Method, "not an upper bound") || r.Context.ModelCapacitySource == "" || r.Context.Loaded.ObservedAt.Before(*r.Context.ModelCapacityObservedAt) {
		t.Fatalf("context %+v", r.Context)
	}
	f.mu.Lock()
	requests := append([]chatRequest(nil), f.requests...)
	f.mu.Unlock()
	if len(requests) != 2 || len(requests[0].Messages) != 0 {
		t.Fatal(requests)
	}
	want := []message{{"system", "role\n\nsystem"}, {"user", "old prompt"}, {"assistant", "retained partial"}, {"user", " user prompt "}}
	got := requests[1]
	if !reflect.DeepEqual(got.Messages, want) || got.Model != modelID+":local" || !got.Stream || got.Truncate || got.Shift || got.Think || got.Options["num_ctx"] != float64(4096) || got.Options["num_predict"] != float64(64) || got.Options["seed"] != float64(42) || got.Options["temperature"] != 0.2 {
		t.Fatalf("translation: %+v", got)
	}
	if f.shows.Load() != 3 {
		t.Fatal("details not refreshed around load")
	}
	// A subsequent fresh call carries no prior messages and explicitly overrides
	// a model's default system even when materialized system is empty.
	fresh := input()
	fresh.Instructions.System = ""
	if _, err = b.Generate(context.Background(), fresh, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got = f.requests[len(f.requests)-1]
	if len(got.Messages) != 2 || got.Messages[0] != (message{Role: "system"}) {
		t.Fatal(got.Messages)
	}
}

func TestModelAndBudgetRejections(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fixture, *core.InferenceInput)
		code   core.ErrorCode
		loads  int
	}{
		{"version", func(f *fixture, _ *core.InferenceInput) { f.version = "0.18.2" }, core.ErrUnsupported, 0},
		{"future_version", func(f *fixture, _ *core.InferenceInput) { f.version = "0.99.0" }, core.ErrUnsupported, 0},
		{"missing", func(_ *fixture, in *core.InferenceInput) { in.Request.Model = "absent:latest" }, core.ErrInvalidRequest, 0},
		{"embedding", func(f *fixture, _ *core.InferenceInput) {
			f.show.(map[string]any)["capabilities"] = []string{"embedding"}
		}, core.ErrUnsupported, 0},
		{"unknown_capability", func(f *fixture, _ *core.InferenceInput) { delete(f.show.(map[string]any), "capabilities") }, core.ErrUnsupported, 0},
		{"cloud_name", func(_ *fixture, in *core.InferenceInput) { in.Request.Model = "tiny:latest-cloud" }, core.ErrUnsupported, 0},
		{"cloud_tag", func(f *fixture, _ *core.InferenceInput) {
			f.tags = map[string]any{"models": []any{map[string]any{"name": modelID, "remote_host": "https://SECRET"}}}
		}, core.ErrUnsupported, 0},
		{"cloud_show", func(f *fixture, _ *core.InferenceInput) { f.show.(map[string]any)["remote_model"] = "remote" }, core.ErrUnsupported, 0},
		{"preset_history", func(f *fixture, _ *core.InferenceInput) {
			f.show.(map[string]any)["messages"] = []any{map[string]string{"role": "user", "content": "hidden"}}
		}, core.ErrUnsupported, 0},
		{"non_gguf", func(f *fixture, _ *core.InferenceInput) {
			f.show.(map[string]any)["details"] = map[string]string{"format": "mlx"}
		}, core.ErrUnsupported, 0},
		{"renderer", func(f *fixture, _ *core.InferenceInput) { f.show.(map[string]any)["renderer"] = "unknown" }, core.ErrUnsupported, 0},
		{"capacity", func(_ *fixture, in *core.InferenceInput) { *in.Request.Options.ContextTokens = 9000 }, core.ErrInvalidRequest, 0},
		{"output_reserve", func(_ *fixture, in *core.InferenceInput) { in.Request.Options.MaxOutputTokens = 4090 }, core.ErrInvalidRequest, 0},
		{"history_budget", func(_ *fixture, in *core.InferenceInput) {
			in.Instructions.History = []core.Message{{Role: core.MessageUser, Content: strings.Repeat("x", 17000)}}
		}, core.ErrInvalidRequest, 0},
		{"template_budget", func(f *fixture, _ *core.InferenceInput) {
			f.show.(map[string]any)["template"] = strings.Repeat("x", 17000)
			f.show.(map[string]any)["modelfile"] = "FROM /local/weights\nTEMPLATE " + strings.Repeat("x", 17000) + "\n"
		}, core.ErrInvalidRequest, 0},
		{"loaded_budget", func(f *fixture, _ *core.InferenceInput) {
			f.ps = map[string]any{"models": []any{map[string]any{"name": modelID, "digest": "digest", "context_length": 64}}}
		}, core.ErrInvalidRequest, 1},
		{"unknown_allocation", func(f *fixture, in *core.InferenceInput) {
			in.Request.Options.ContextTokens = nil
			f.ps = map[string]any{"models": []any{}}
		}, core.ErrUnsupported, 1},
		{"seed_wrap", func(_ *fixture, in *core.InferenceInput) {
			s := int64(math.MaxUint32) + 1
			in.Request.Options.Seed = &s
		}, core.ErrUnsupported, 0},
		{"temperature_overflow", func(_ *fixture, in *core.InferenceInput) { v := math.MaxFloat64; in.Request.Options.Temperature = &v }, core.ErrUnsupported, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := defaults()
			in := input()
			tc.change(f, &in)
			b, _ := f.server(t)
			_, err := b.Generate(context.Background(), in, func(string) error { t.Error("unexpected callback"); return nil })
			requireCode(t, err, tc.code)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != tc.loads {
				t.Fatalf("unexpected inference: %v", f.requests)
			}
		})
	}
}

func TestUnknownMetadataAndDefaultAllocation(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			f := defaults()
			delete(f.show.(map[string]any), "model_info")
			if explicit {
				f.ps = map[string]any{"models": []any{map[string]any{"name": modelID, "digest": "different", "context_length": 999}}}
			}
			b, _ := f.server(t)
			in := input()
			if !explicit {
				in.Request.Options.ContextTokens = nil
			}
			r, err := b.Generate(context.Background(), in, func(string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if r.Context.ModelCapacityTokens != nil || r.Context.ModelCapacityObservedAt != nil || r.EffectiveOptions != nil || (r.Context.Loaded == nil) != explicit || (r.Context.RequestedTokens != nil) != explicit {
				t.Fatalf("unknown values invented: %+v", r)
			}
		})
	}
}

func TestStreamFailures(t *testing.T) {
	cases := []struct {
		name, wire string
		code       core.ErrorCode
	}{
		{"empty", "", core.ErrBackendFailure},
		{"partial_eof", partial, core.ErrBackendFailure},
		{"malformed", partial + "{oops}\n", core.ErrBackendFailure},
		{"cut_json", partial + `{"done":tru`, core.ErrBackendFailure},
		{"server_error", partial + `{"error":"SECRET prompt"}` + "\n", core.ErrBackendFailure},
		{"oversized_input", `{"error":"the input length exceeds the context length"}` + "\n", core.ErrInvalidRequest},
		{"missing_done", `{"model":"tiny:latest","message":{"role":"assistant","content":"bad"}}` + "\n", core.ErrBackendFailure},
		{"duplicate", strings.Replace(terminal, `"done":true`, `"done":false,"done":true`, 1), core.ErrBackendFailure},
		{"negative_metric", strings.Replace(terminal, `"eval_count":2`, `"eval_count":-2`, 1), core.ErrBackendFailure},
		{"model_mismatch", strings.Replace(terminal, modelID, "different", 1), core.ErrBackendFailure},
		{"remote", strings.Replace(terminal, `"done":true`, `"done":true,"remote_host":"https://SECRET"`, 1), core.ErrBackendFailure},
		{"missing_reason", strings.Replace(terminal, `"done_reason":"stop",`, "", 1), core.ErrBackendFailure},
		{"trailing_error", terminal + `{"error":"SECRET"}`, core.ErrBackendFailure},
		{"oversized_record", partial + strings.Repeat("x", recordLimit+1), core.ErrLimitExceeded},
		{"invalid_utf8", partial + "{\"bad\":\"\xff\"}", core.ErrBackendFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := defaults()
			f.chat = func(w http.ResponseWriter, _ *http.Request, _ chatRequest) { io.WriteString(w, tc.wire) }
			b, _ := f.server(t)
			var text strings.Builder
			r, err := b.Generate(context.Background(), input(), func(s string) error { text.WriteString(s); return nil })
			requireCode(t, err, tc.code)
			if r.Text != "" {
				t.Fatal("adapter duplicated text")
			}
			if strings.HasPrefix(tc.wire, partial) && text.String() != "partial" {
				t.Fatal(text.String())
			}
		})
	}
}

func TestStreamLimitsAndUnknownMetrics(t *testing.T) {
	for _, kind := range []string{"output", "wire", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			f := defaults()
			f.chat = func(w http.ResponseWriter, _ *http.Request, _ chatRequest) {
				if kind == "unknown" {
					io.WriteString(w, `{"model":"tiny:latest","message":{"role":"assistant","content":""},"done":true,"done_reason":"length"}`)
					return
				}
				field := "content"
				limit := core.MaxOutputBytes
				if kind == "wire" {
					field = "thinking"
					limit = streamLimit
				}
				line := fmt.Sprintf(`{"model":"tiny:latest","message":{"role":"assistant","%s":"%s"},"done":false}`+"\n", field, strings.Repeat("a", 64<<10))
				for i := 0; i < limit/(64<<10)+2; i++ {
					if _, err := io.WriteString(w, line); err != nil {
						return
					}
				}
			}
			b, _ := f.server(t)
			r, err := b.Generate(context.Background(), input(), func(string) error { return nil })
			if kind == "unknown" {
				if err != nil || r.Usage.InputTokens != nil || r.Usage.OutputTokens != nil || r.Usage.TotalDurationNS != nil {
					t.Fatalf("%+v %v", r, err)
				}
			} else {
				requireCode(t, err, core.ErrLimitExceeded)
			}
		})
	}
}

func TestCancellationCallbackAndTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "callback", "timeout", "client_timeout"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan struct{})
			f := defaults()
			f.chat = func(w http.ResponseWriter, r *http.Request, _ chatRequest) {
				io.WriteString(w, partial)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			}
			b, _ := f.server(t)
			if mode == "timeout" {
				b.timeout = 100 * time.Millisecond
			}
			if mode == "client_timeout" {
				b.client.Timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			sentinel := errors.New("callback stopped")
			start := time.Now()
			_, err := b.Generate(ctx, input(), func(string) error {
				calls++
				if mode == "cancel" {
					cancel()
				}
				if mode == "callback" {
					return sentinel
				}
				return nil
			})
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if mode == "callback" {
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			} else {
				requireCode(t, err, core.ErrBackendUnavailable)
			}
			if calls != 1 || time.Since(start) > 2*time.Second {
				t.Fatalf("return/callback count %d", calls)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("server did not observe closed transport")
			}
		})
	}
}

func TestHTTPFaultsAndRedirects(t *testing.T) {
	for _, status := range []int{400, 401, 404, 422, 429, 500, 502, 503, 504, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var leaks atomic.Int32
			external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks.Add(1) }))
			defer external.Close()
			f := defaults()
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/chat" {
					return false
				}
				w.Header().Set("Location", external.URL)
				w.WriteHeader(status)
				io.WriteString(w, "SECRET prompt")
				return true
			}
			b, _ := f.server(t)
			_, err := b.Generate(context.Background(), input(), func(string) error { return nil })
			code := core.ErrBackendFailure
			if status == 400 || status == 404 || status == 422 {
				code = core.ErrInvalidRequest
			}
			if status == 429 || status == 502 || status == 503 || status == 504 {
				code = core.ErrBackendUnavailable
			}
			requireCode(t, err, code)
			if leaks.Load() != 0 {
				t.Fatal("redirect sent request elsewhere")
			}
		})
	}
	for _, endpoint := range []string{"https://127.0.0.1:11434", "http://example.com", "http://192.168.1.1:11434", "http://user:pass@127.0.0.1", "http://127.0.0.1/api", "http://127.0.0.1?x=1"} {
		if _, err := New(Config{Endpoint: endpoint}); err == nil {
			t.Fatal("accepted endpoint", endpoint)
		}
	}
	f := defaults()
	b, s := f.server(t)
	s.Close()
	_, err := b.Models(context.Background())
	requireCode(t, err, core.ErrBackendUnavailable)
}

func TestMetadataProtocolAndCloudDiscovery(t *testing.T) {
	for _, body := range []string{"null", `{"models":null}`, "{", strings.Repeat("x", metadataLimit+1)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			f := defaults()
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/version" {
					return false
				}
				io.WriteString(w, body)
				return true
			}
			b, _ := f.server(t)
			if _, err := b.Models(context.Background()); err == nil {
				t.Fatal("accepted malformed version")
			}
		})
	}
	f := defaults()
	f.tags = map[string]any{"models": []any{map[string]any{"name": "remote:cloud", "remote_host": "https://SECRET"}}}
	b, _ := f.server(t)
	models, err := b.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Capabilities.TextGeneration || f.shows.Load() != 0 {
		t.Fatalf("cloud discovery: %+v %v", models, err)
	}
}

func TestRefreshIdentityAndObservations(t *testing.T) {
	for _, mode := range []string{"digest", "metadata", "observed", "unloaded"} {
		t.Run(mode, func(t *testing.T) {
			f := defaults()
			var tags, shows, ps atomic.Int32
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/api/tags":
					if tags.Add(1) == 2 && mode == "digest" {
						io.WriteString(w, `{"models":[{"name":"tiny:latest","digest":"changed"}]}`)
						return true
					}
				case "/api/show":
					if shows.Add(1) == 2 && mode == "metadata" {
						io.WriteString(w, `{"details":{"format":"gguf"},"template":"template","modelfile":"FROM /local/weights\nTEMPLATE template\n","capabilities":["completion"],"model_info":{"general.architecture":"test","test.context_length":100}}`)
						return true
					}
				case "/api/ps":
					if ps.Add(1) == 2 {
						if mode == "unloaded" {
							io.WriteString(w, `{"models":[]}`)
							return true
						}
						if mode == "observed" {
							io.WriteString(w, `{"models":[{"name":"tiny:latest","digest":"digest","context_length":2048}]}`)
							return true
						}
					}
				}
				return false
			}
			b, _ := f.server(t)
			r, err := b.Generate(context.Background(), input(), func(string) error { return nil })
			switch mode {
			case "digest":
				requireCode(t, err, core.ErrBackendFailure)
			case "metadata":
				requireCode(t, err, core.ErrInvalidRequest)
			case "unloaded":
				if err != nil || r.Context.Loaded != nil {
					t.Fatalf("stale observation retained: %+v %v", r, err)
				}
			case "observed":
				if err != nil || r.Context.Loaded.Tokens != 2048 || *r.Context.RequestedTokens != 4096 {
					t.Fatalf("observed/requested conflated: %+v %v", r, err)
				}
			}
		})
	}
}

func TestBrokenTransportAfterTerminal(t *testing.T) {
	f := defaults()
	f.chat = func(w http.ResponseWriter, _ *http.Request, _ chatRequest) {
		w.Header().Set("Content-Length", fmt.Sprint(len(terminal)+100))
		io.WriteString(w, terminal)
	}
	b, _ := f.server(t)
	_, err := b.Generate(context.Background(), input(), func(string) error { return nil })
	requireCode(t, err, core.ErrBackendFailure)
}

func TestClientLocalPolicyAndOwnership(t *testing.T) {
	f := defaults()
	_, s := f.server(t)
	var forbidden atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(*http.Request) (*url.URL, error) { forbidden.Add(1); return url.Parse("http://invalid.example") }
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { forbidden.Add(1); return nil }}
	b, err := New(Config{Endpoint: s.URL, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseIdleConnections()
	if _, err = b.Generate(context.Background(), input(), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if forbidden.Load() != 0 || client.Transport != transport || transport.Proxy == nil || client.CheckRedirect == nil || client.Timeout != time.Second {
		t.Fatal("client policy or ownership")
	}
}

func TestCancellationDuringPreflightAndModelsTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			closed := make(chan struct{})
			f := defaults()
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/version" {
					return false
				}
				close(entered)
				<-r.Context().Done()
				close(closed)
				return true
			}
			b, _ := f.server(t)
			b.timeout = 250 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := b.Models(ctx); result <- err }()
			<-entered
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-result:
				if mode == "cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				} else {
					requireCode(t, err, core.ErrBackendUnavailable)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("preflight body not closed")
			}
		})
	}
}
