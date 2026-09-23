package ollama

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"unicode/utf8"

	"taskworker.local/taskworker/internal/core"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model    string         `json:"model"`
	Messages []message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Truncate bool           `json:"truncate"`
	Shift    bool           `json:"shift"`
	Think    bool           `json:"think"`
	Options  map[string]any `json:"options"`
}
type record struct {
	Model       string `json:"model"`
	RemoteHost  string `json:"remote_host"`
	RemoteModel string `json:"remote_model"`
	Message     *struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		// Thinking and tool calls are deliberately not interpreted or emitted.
	} `json:"message"`
	Done       *bool   `json:"done"`
	Reason     string  `json:"done_reason"`
	Error      *string `json:"error"`
	Input      *int    `json:"prompt_eval_count"`
	Output     *int    `json:"eval_count"`
	TotalNS    *int64  `json:"total_duration"`
	LoadNS     *int64  `json:"load_duration"`
	GenerateNS *int64  `json:"eval_duration"`
}

func (b *Backend) Generate(parent context.Context, in core.InferenceInput, emit func(string) error) (result core.Result, err error) {
	ctx, cancel := context.WithTimeout(parent, b.timeout)
	defer cancel()
	defer func() {
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fault(core.ErrBackendUnavailable)
		}
	}()
	if emit == nil {
		return result, fault(core.ErrInvalidRequest)
	}
	if err = core.ValidateRequest(in.Request, in.Instructions); err != nil {
		var f *core.Fault
		if errors.As(err, &f) && f.Code == core.ErrLimitExceeded {
			return result, fault(core.ErrLimitExceeded)
		}
		return result, fault(core.ErrInvalidRequest)
	}
	o := in.Request.Options
	if (o.Temperature != nil && *o.Temperature > math.MaxFloat32) || (o.Seed != nil && (*o.Seed < 0 || *o.Seed > math.MaxUint32)) {
		return result, fault(core.ErrUnsupported)
	}
	if err = b.version(ctx); err != nil {
		return result, err
	}
	m, d, info, err := b.selectModel(ctx, in.Request.Model)
	if err != nil {
		return result, err
	}
	info.RequestedTokens = clone(o.ContextTokens)
	info.ReservedOutputTokens = o.MaxOutputTokens
	info.InputEstimate = estimate(in.Instructions, d.Template)
	if err = budget(info); err != nil {
		return result, err
	}
	req := chatRequest{Model: m.Name + ":local", Messages: []message{}, Options: map[string]any{"num_predict": o.MaxOutputTokens}}
	if o.ContextTokens != nil {
		req.Options["num_ctx"] = *o.ContextTokens
	}
	if o.Temperature != nil {
		req.Options["temperature"] = *o.Temperature
	}
	if o.Seed != nil {
		req.Options["seed"] = *o.Seed
	}
	// Empty messages loads the model without running a prompt. Never unload: the
	// daemon may be shared. This gives /api/ps a meaningful post-load observation.
	var load record
	if err = b.json(ctx, "/api/chat", req, &load); err != nil {
		return result, err
	}
	if load.Done == nil || !*load.Done || load.Reason != "load" || load.Error != nil || load.RemoteHost != "" || load.RemoteModel != "" || !sameModel(load.Model, m.Name) {
		return result, fault(core.ErrBackendFailure)
	}
	// Recheck identity and details after loading; a manifest can change externally.
	m2, d2, refreshed, err := b.selectModel(ctx, m.Name)
	if err != nil {
		return result, err
	}
	if m.Digest == "" || m2.Digest != m.Digest {
		return result, fault(core.ErrBackendFailure)
	}
	info.ModelCapacityTokens = refreshed.ModelCapacityTokens
	info.ModelCapacitySource = refreshed.ModelCapacitySource
	info.ModelCapacityObservedAt = refreshed.ModelCapacityObservedAt
	info.InputEstimate = estimate(in.Instructions, d2.Template)
	info.Loaded, err = b.loaded(ctx, m)
	if err != nil {
		return result, err
	}
	if err = budget(info); err != nil {
		return result, err
	}
	if info.RequestedTokens == nil && info.Loaded == nil {
		return result, fault(core.ErrUnsupported)
	}
	// Always send system, even empty, to override the Modelfile's default system.
	req.Messages = append(req.Messages, message{Role: "system", Content: in.Instructions.System})
	for _, h := range in.Instructions.History {
		req.Messages = append(req.Messages, message{Role: string(h.Role), Content: h.Content})
	}
	req.Messages = append(req.Messages, message{Role: "user", Content: in.Instructions.Prompt})
	req.Stream = true
	result, err = b.stream(ctx, req, m.Name, emit)
	if err != nil {
		return core.Result{}, err
	}
	info.Loaded, err = b.loaded(ctx, m)
	if err != nil {
		return core.Result{}, err
	}
	result.Context = info
	// The engine does not report resolved sampling options; keep them unknown.
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > core.MetadataReserve/2 {
		return core.Result{}, fault(core.ErrLimitExceeded)
	}
	if err = ctx.Err(); err != nil {
		return core.Result{}, err
	}
	return result, nil
}

func clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func estimate(in core.Instructions, template string) *core.TokenEstimate {
	n := len(in.System) + len(in.Prompt) + len(template)
	for _, h := range in.History {
		n += len(h.Content)
	}
	return &core.TokenEstimate{Tokens: (n+3)/4 + 16*(len(in.History)+2), Method: "heuristic: ceil((UTF-8 input bytes + template source bytes)/4) + 16 per message; not an upper bound", Exact: false}
}

func budget(info core.ContextInfo) error {
	reserve := info.ReservedOutputTokens
	if info.ModelCapacityTokens != nil {
		if reserve >= *info.ModelCapacityTokens || (info.RequestedTokens != nil && *info.RequestedTokens > *info.ModelCapacityTokens) {
			return fault(core.ErrInvalidRequest)
		}
	}
	// Avoid overflow by subtracting the required output reservation first.
	check := func(n int) bool { return reserve >= n || info.InputEstimate.Tokens > n-reserve }
	if info.ModelCapacityTokens != nil && check(*info.ModelCapacityTokens) {
		return fault(core.ErrInvalidRequest)
	}
	if info.RequestedTokens != nil && check(*info.RequestedTokens) {
		return fault(core.ErrInvalidRequest)
	}
	if info.Loaded != nil && check(info.Loaded.Tokens) {
		return fault(core.ErrInvalidRequest)
	}
	return nil
}

func sameModel(reported, id string) bool { return reported == id || reported == id+":local" }

func (b *Backend) stream(ctx context.Context, req chatRequest, id string, emit func(string) error) (core.Result, error) {
	var result core.Result
	resp, err := b.request(ctx, "/api/chat", req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	// Both individual records and the whole wire response (including discarded
	// thinking/tools) are bounded. A terminal record AND clean EOF are required.
	r := bufio.NewReaderSize(io.LimitReader(resp.Body, streamLimit+1), recordLimit+1)
	total, output := 0, 0
	done := false
	for {
		line, readErr := r.ReadSlice('\n')
		total += len(line)
		if len(line) > recordLimit || total > streamLimit || errors.Is(readErr, bufio.ErrBufferFull) {
			return result, fault(core.ErrLimitExceeded)
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if len(line) != 0 {
			if done || !validObject(line) {
				return result, fault(core.ErrBackendFailure)
			}
			var v record
			if json.Unmarshal(line, &v) != nil {
				return result, fault(core.ErrBackendFailure)
			}
			if v.Error != nil {
				// This exact runner error is verified in the pinned source. No raw
				// error text is returned; all other streamed errors fail closed.
				if strings.TrimSpace(*v.Error) == "the input length exceeds the context length" {
					return result, fault(core.ErrInvalidRequest)
				}
				return result, fault(core.ErrBackendFailure)
			}
			if v.Done == nil || v.Message == nil || v.Message.Role != "assistant" || !sameModel(v.Model, id) || v.RemoteHost != "" || v.RemoteModel != "" || !validMetrics(v) {
				return result, fault(core.ErrBackendFailure)
			}
			if *v.Done && v.Reason != "stop" && v.Reason != "length" {
				return result, fault(core.ErrBackendFailure)
			}
			output += len(v.Message.Content)
			if output > core.MaxOutputBytes {
				return result, fault(core.ErrLimitExceeded)
			}
			if v.Message.Content != "" {
				if err := emit(v.Message.Content); err != nil {
					return result, err
				}
			}
			if *v.Done {
				done = true
				result = core.Result{FinishReason: v.Reason, EffectiveModel: v.Model, Usage: core.Usage{InputTokens: v.Input, OutputTokens: v.Output, TotalDurationNS: v.TotalNS, LoadDurationNS: v.LoadNS, GenerateDurationNS: v.GenerateNS}}
			}
		}
		if readErr != nil {
			if readErr == io.EOF && done {
				return result, nil
			}
			var timeout net.Error
			if errors.As(readErr, &timeout) && timeout.Timeout() {
				return result, fault(core.ErrBackendUnavailable)
			}
			return result, fault(core.ErrBackendFailure)
		}
	}
}

func validMetrics(v record) bool {
	return (v.Input == nil || *v.Input >= 0) && (v.Output == nil || *v.Output >= 0) && (v.TotalNS == nil || *v.TotalNS >= 0) && (v.LoadNS == nil || *v.LoadNS >= 0) && (v.GenerateNS == nil || *v.GenerateNS >= 0)
}

// Reject duplicate keys and excessive JSON nesting, including in ignored fields.
// Unknown fields remain forward compatible within the explicitly audited version.
func validObject(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		tok, err := d.Token()
		if err != nil {
			return false
		}
		delim, isDelim := tok.(json.Delim)
		if !isDelim {
			return depth > 0
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				key, ok := k.(string)
				if err != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			if depth == 0 {
				return false
			}
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
