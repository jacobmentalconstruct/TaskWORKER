// Package httpapi owns the version 1 local HTTP protocol, not inference lifetime.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"taskworker.local/taskworker/internal/adapters/webui"
	"taskworker.local/taskworker/internal/core"
)

// Endpoint permits only explicit loopback HTTP endpoints. localhost is pinned.
func Endpoint(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, invalid()
	}
	h := u.Hostname()
	if h == "localhost" {
		h = "127.0.0.1"
	}
	ip := net.ParseIP(h)
	if ip == nil || !ip.IsLoopback() {
		return nil, invalid()
	}
	p := u.Port()
	if p == "" {
		p = "80"
	}
	n, e := strconv.Atoi(p)
	if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
		return nil, invalid()
	}
	u.Host = net.JoinHostPort(ip.String(), p)
	u.Path = ""
	return u, nil
}

type Handler struct {
	Service      core.Service
	Host         string
	WriteTimeout time.Duration
	Heartbeat    time.Duration
	slots        chan struct{}
}

func NewHandler(s core.Service, host string) *Handler {
	return &Handler{Service: s, Host: host, WriteTimeout: 10 * time.Second, Heartbeat: 15 * time.Second, slots: make(chan struct{}, 32)}
}
func Server(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}

func PublicFault(err error) *core.Fault {
	var f *core.Fault
	if errors.As(err, &f) && f != nil {
		if _, ok := statuses[f.Code]; ok {
			return &core.Fault{Code: f.Code, Message: string(f.Code), Retryable: f.Retryable}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &core.Fault{Code: core.ErrUnavailable, Message: "request ended; acceptance may be uncertain", Retryable: true}
	}
	return &core.Fault{Code: core.ErrInternal, Message: "service operation failed"}
}

var statuses = map[core.ErrorCode]int{
	core.ErrInvalidRequest: 400, core.ErrNotFound: 404, core.ErrConflict: 409, core.ErrQueueFull: 429, core.ErrLimitExceeded: 413, core.ErrUnsupported: 422,
	core.ErrBackendUnavailable: 503, core.ErrBackendFailure: 502, core.ErrCancelled: 409, core.ErrInterrupted: 409, core.ErrCursorExpired: 410, core.ErrCursorInvalid: 400,
	core.ErrSlowConsumer: 429, core.ErrStorage: 503, core.ErrUnavailable: 503, core.ErrInternal: 500,
}

func fail(w http.ResponseWriter, e error) { f := PublicFault(e); writeJSON(w, statuses[f.Code], f) }
func writeJSON(w http.ResponseWriter, status int, v any) {
	// Operation latency must not consume the response write budget.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func protocol(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, &core.Fault{Code: core.ErrInvalidRequest, Message: message})
}

// sameAuthority compares an HTTP authority with the explicit socket address.
// Only HTTP's default port may be omitted; host spelling and all other ports
// remain exact. In particular, this does not equate distinct loopback sites.
func sameAuthority(authority, socketAddress string) bool {
	if authority == socketAddress {
		return true
	}
	_, port, err := net.SplitHostPort(socketAddress)
	return err == nil && port == "80" && authority == strings.TrimSuffix(socketAddress, ":80")
}

func sameOrigin(origin, socketAddress string) bool {
	authority, ok := strings.CutPrefix(origin, "http://")
	return ok && sameAuthority(authority, socketAddress)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(h.WriteTimeout))
	if !sameAuthority(r.Host, h.Host) {
		protocol(w, 403, "Host rejected")
		return
	}
	origins, has := r.Header["Origin"]
	if has && (len(origins) != 1 || !sameOrigin(origins[0], h.Host)) {
		protocol(w, 403, "Origin rejected")
		return
	}
	// Sec-Fetch-Site also protects browser requests where Origin is omitted.
	if values, present := r.Header["Sec-Fetch-Site"]; present && (len(values) != 1 || (values[0] != "same-origin" && values[0] != "none")) {
		protocol(w, 403, "cross-site request rejected")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		fail(w, &core.Fault{Code: core.ErrUnavailable, Retryable: true})
		return
	}
	if r.URL.RawPath != "" {
		protocol(w, 400, "encoded path rejected")
		return
	}
	path := r.URL.Path
	if body, contentType, ok := webui.Asset(path); ok {
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			protocol(w, 405, "method rejected")
			return
		}
		if r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			protocol(w, 400, "query or body rejected")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(body)
		return
	}
	method := "GET"
	known := true
	switch path {
	case "/v1/health", "/v1/models", "/v1/snapshot", "/v1/jobs", "/v1/queue", "/v1/events":
	case "/v1/submit", "/v1/retry", "/v1/branch", "/v1/queue/pause", "/v1/queue/resume":
		method = "POST"
	default:
		parts := strings.Split(strings.TrimPrefix(path, "/v1/jobs/"), "/")
		if !strings.HasPrefix(path, "/v1/jobs/") || len(parts) > 2 || !jobID(parts[0]) {
			known = false
		} else if len(parts) == 2 {
			if parts[1] == "cancel" {
				method = "POST"
			} else if parts[1] != "result" {
				known = false
			}
		}
	}
	if !known {
		fail(w, &core.Fault{Code: core.ErrNotFound})
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		protocol(w, 405, "method rejected")
		return
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || (path != "/v1/events" && len(q) != 0) {
		protocol(w, 400, "query rejected")
		return
	}
	if path == "/v1/events" {
		h.events(w, r, q)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var v any
	var err error
	if method == "GET" {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			protocol(w, 400, "body rejected")
			return
		}
		switch path {
		case "/v1/health":
			v = map[string]string{"status": "serving", "backend": "not_checked"}
		case "/v1/models":
			v, err = h.Service.Models(ctx)
		case "/v1/snapshot", "/v1/jobs", "/v1/queue":
			var s core.Snapshot
			s, err = h.Service.Snapshot(ctx)
			v = s
			if path == "/v1/jobs" {
				v = s.Jobs
			}
			if path == "/v1/queue" {
				v = s.Queue
			}
		default:
			parts := strings.Split(strings.TrimPrefix(path, "/v1/jobs/"), "/")
			v, err = h.Service.GetJob(ctx, core.JobID(parts[0]))
		}
	} else {
		if len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 {
			protocol(w, 415, "one application/json Content-Type and no Content-Encoding required")
			return
		}
		media, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || media != "application/json" || (len(params) > 0 && (len(params) != 1 || strings.ToLower(params["charset"]) != "utf-8")) {
			protocol(w, 415, "application/json required")
			return
		}
		b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if e != nil {
			fail(w, &core.Fault{Code: core.ErrLimitExceeded})
			return
		}
		switch path {
		case "/v1/submit":
			var c core.SubmitCommand
			err = DecodeCommand(b, &c)
			if err == nil {
				v, err = h.Service.Submit(ctx, c)
			}
		case "/v1/retry":
			var c core.RetryCommand
			err = DecodeCommand(b, &c)
			if err == nil {
				v, err = h.Service.Retry(ctx, c)
			}
		case "/v1/branch":
			var c core.BranchCommand
			err = DecodeCommand(b, &c)
			if err == nil {
				v, err = h.Service.Branch(ctx, c)
			}
		default:
			var c struct{}
			err = DecodeCommand(b, &c)
			if err == nil {
				if strings.HasPrefix(path, "/v1/queue/") {
					v, err = h.Service.SetQueuePaused(ctx, path == "/v1/queue/pause")
				} else {
					id := strings.Split(strings.TrimPrefix(path, "/v1/jobs/"), "/")[0]
					v, err = h.Service.Cancel(ctx, core.JobID(id))
				}
			}
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func jobID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (h *Handler) events(w http.ResponseWriter, r *http.Request, q url.Values) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || len(q) > 1 || (len(q) == 1 && len(q["after"]) != 1) {
		protocol(w, 400, "event query or body rejected")
		return
	}
	headers := r.Header.Values("Last-Event-ID")
	if len(headers) > 1 {
		fail(w, &core.Fault{Code: core.ErrCursorInvalid})
		return
	}
	after, querySet := q["after"]
	raw := ""
	if querySet {
		raw = after[0]
	}
	if len(headers) == 1 {
		if querySet && raw != headers[0] {
			fail(w, &core.Fault{Code: core.ErrCursorInvalid})
			return
		}
		raw = headers[0]
	}
	var snapshot *core.Snapshot
	var cursor core.Cursor
	var err error
	if raw == "" && (querySet || len(headers) > 0) {
		fail(w, &core.Fault{Code: core.ErrCursorInvalid})
		return
	}
	if raw == "" {
		s, e := h.Service.Snapshot(r.Context())
		err = e
		snapshot = &s
		cursor = s.Cursor
	} else {
		cursor, err = ParseCursor(raw)
	}
	if err != nil {
		fail(w, err)
		return
	}
	stream, err := h.Service.Watch(r.Context(), cursor)
	if err != nil {
		fail(w, err)
		return
	}
	defer stream.Close()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(kind, id string, v any) error {
		_ = rc.SetWriteDeadline(time.Now().Add(h.WriteTimeout))
		if id != "" {
			if _, e := io.WriteString(w, "id: "+id+"\n"); e != nil {
				return e
			}
		}
		if _, e := io.WriteString(w, "event: "+kind+"\ndata: "); e != nil {
			return e
		}
		if e := json.NewEncoder(w).Encode(v); e != nil {
			return e
		}
		if _, e := io.WriteString(w, "\n"); e != nil {
			return e
		}
		return rc.Flush()
	}
	if snapshot != nil {
		if send("snapshot", CursorID(cursor), snapshot) != nil {
			return
		}
	} else {
		if _, err = io.WriteString(w, ": connected\n\n"); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
	for {
		ctx, cancel := context.WithTimeout(r.Context(), h.Heartbeat)
		ev, e := stream.Next(ctx)
		cancel()
		if errors.Is(e, context.DeadlineExceeded) && r.Context().Err() == nil {
			_ = rc.SetWriteDeadline(time.Now().Add(h.WriteTimeout))
			if _, e = io.WriteString(w, ": heartbeat\n\n"); e != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
			continue
		}
		if e != nil {
			if e != io.EOF && r.Context().Err() == nil {
				_ = send("error", "", PublicFault(e))
			}
			return
		}
		if send("event", CursorID(ev.Cursor), ev) != nil {
			return
		}
	}
}
