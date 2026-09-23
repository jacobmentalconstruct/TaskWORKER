package mcpbridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The MCP stdio transport is newline-delimited: one JSON-RPC message per line
// with no embedded newlines. The SDK reads stdin as a stream of JSON values, so
// a malformed or truncated line would swallow the following line and end the
// session without a reply. filterInput enforces the line framing instead: each
// line is bounded and syntactically checked before the SDK sees it, and a bad
// line gets a JSON-RPC error with a null id (parse error -32700, invalid
// request -32600) while the session continues. Valid lines pass through
// byte-for-byte; message semantics stay entirely with the SDK.
//
// "Bad" includes valid JSON that is not a JSON-RPC message: the SDK treats an
// envelope it cannot decode (missing or wrong "jsonrpc", a non-string method,
// an object or boolean id, an empty batch, extreme nesting) as a fatal
// transport error and ends the session with exit code 1 and no reply, so
// envelopeProblem screens for those first. It also rejects ids the SDK cannot
// return exactly: it normalizes ids through a 64-bit float/integer, which would
// silently answer id 9007199254740993 as ...992 and 1.5 as 1.

const (
	maxJSONDepth = 512       // the SDK's own limit is 1000; nothing this bridge accepts nests beyond a handful
	maxSafeID    = 1<<53 - 1 // largest integer id returned exactly (the JSON interoperable range)
)

var integerID = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// envelopeProblem says why line, which is already valid JSON starting with '{'
// or '[', must not be passed to the SDK; "" means it is a JSON-RPC message (or
// a batch of them) that the SDK can decode and answer.
func envelopeProblem(line []byte) string {
	if jsonDepth(line) > maxJSONDepth {
		return fmt.Sprintf("nesting is deeper than %d levels", maxJSONDepth)
	}
	if line[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(line, &batch) != nil {
			return "a batch must be an array of messages"
		}
		if len(batch) == 0 {
			return "an empty batch is not a message"
		}
		for _, m := range batch {
			if p := messageProblem(m); p != "" {
				return p
			}
		}
		return ""
	}
	return messageProblem(line)
}

func messageProblem(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return "a message must be a JSON object"
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "a message must be a JSON object"
	}
	var version string
	if json.Unmarshal(m["jsonrpc"], &version) != nil || version != "2.0" {
		return `"jsonrpc" must be the string "2.0"`
	}
	method, hasMethod := m["method"]
	if hasMethod {
		var s string
		// null unmarshals into a string without error, so require a JSON string.
		if !bytes.HasPrefix(bytes.TrimSpace(method), []byte(`"`)) || json.Unmarshal(method, &s) != nil {
			return `"method" must be a string`
		}
	}
	id, hasID := m["id"]
	if hasID {
		if p := idProblem(id); p != "" {
			return p
		}
	}
	if !hasMethod && !hasID {
		return `a message needs a "method" or an "id"`
	}
	// Without a method it is a response, and the SDK aborts the session on one
	// that has no id to correlate (an id of null is a request's, never a reply's).
	if !hasMethod && strings.TrimSpace(string(id)) == "null" {
		return `a response needs a non-null "id"`
	}
	return ""
}

// idProblem accepts null, any string, and integers up to +-(2^53-1) written
// plainly (no fraction, exponent, "-0" or leading zeros).
func idProblem(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "null", strings.HasPrefix(s, `"`):
		return ""
	case integerID.MatchString(s) && s != "-0":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= -maxSafeID && n <= maxSafeID {
			return ""
		}
	}
	return fmt.Sprintf(`"id" must be a string or an integer within +-%d`, int64(maxSafeID))
}

// jsonDepth is the deepest array/object nesting in b (string contents ignored).
func jsonDepth(b []byte) int {
	depth, max := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			if depth++; depth > max {
				max = depth
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return max
}

// framer serializes whole-message writes from the SDK and from the framing
// layer so lines never interleave on stdout. It also enforces the cancellation
// rule the SDK leaves to handlers: after notifications/cancelled for an
// in-flight request, the server MUST NOT send any further message for it, so
// that request's response is dropped. Only ids seen as in-flight requests are
// tracked, so a cancel for a finished request never suppresses a reused id.
type framer struct {
	mu        sync.Mutex
	w         io.Writer
	inflight  map[string]bool
	cancelled map[string]bool
}

func newFramer(w io.Writer) *framer {
	return &framer{w: w, inflight: map[string]bool{}, cancelled: map[string]bool{}}
}

func canonID(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	return b.String()
}

// track records an inbound request or applies an inbound cancellation.
func (f *framer) track(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Method == "notifications/cancelled" {
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil && len(p.RequestID) > 0 {
			if id := canonID(p.RequestID); f.inflight[id] {
				f.cancelled[id] = true
			}
		}
		return
	}
	if len(m.ID) > 0 && string(m.ID) != "null" {
		f.inflight[canonID(m.ID)] = true
	}
}

func (f *framer) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inflight) > 0 {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(p, &m) == nil && m.Method == "" && len(m.ID) > 0 {
			id := canonID(m.ID)
			if f.inflight[id] {
				delete(f.inflight, id)
				if f.cancelled[id] {
					delete(f.cancelled, id)
					return len(p), nil // cancelled: no further message for this request
				}
			}
		}
	}
	return f.w.Write(p)
}

func (f *framer) Close() error { return nil }

func rpcError(code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": code, "message": msg}})
	return append(b, '\n')
}

func filterInput(in io.Reader, out *framer, limit int, note func(string)) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		br := bufio.NewReaderSize(in, 64<<10)
		var line []byte
		oversized := false
		flush := func() error {
			defer func() { line, oversized = line[:0], false }()
			if oversized {
				note("inbound message exceeded the size bound and was discarded")
				_, err := out.Write(rpcError(-32600, fmt.Sprintf("message exceeds the %d byte limit", limit)))
				return err
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				return nil
			}
			if !json.Valid(trimmed) {
				note("inbound line was not valid JSON")
				_, err := out.Write(rpcError(-32700, "Parse error: each line must be one complete JSON-RPC message"))
				return err
			}
			if trimmed[0] != '{' && trimmed[0] != '[' {
				note("inbound line was not a JSON-RPC message")
				_, err := out.Write(rpcError(-32600, "Invalid Request: a message must be a JSON object"))
				return err
			}
			if reason := envelopeProblem(trimmed); reason != "" {
				note("inbound message was not a JSON-RPC message the bridge can pass on")
				_, err := out.Write(rpcError(-32600, "Invalid Request: "+reason))
				return err
			}
			out.track(trimmed) // register before the SDK can answer
			_, err := pw.Write(append(append([]byte(nil), trimmed...), '\n'))
			return err
		}
		for {
			chunk, err := br.ReadSlice('\n')
			if !oversized {
				if len(line)+len(chunk) > limit {
					oversized = true
					line = line[:0]
				} else {
					line = append(line, chunk...)
				}
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if err == io.EOF {
					if len(line) > 0 || oversized {
						if ferr := flush(); ferr != nil {
							pw.CloseWithError(ferr)
							return
						}
					}
					pw.Close()
					return
				}
				pw.CloseWithError(err)
				return
			}
			if ferr := flush(); ferr != nil {
				pw.CloseWithError(ferr)
				return
			}
		}
	}()
	return pr
}
