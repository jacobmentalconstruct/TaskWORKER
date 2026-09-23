package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"taskworker.local/taskworker/internal/core"
)

const (
	metadataLimit = 4 << 20
	recordLimit   = 256 << 10
	streamLimit   = 32 << 20
)

// Config uses a literal loopback HTTP endpoint (or localhost, pinned to IPv4).
// Client is copied, never mutated. Its transport must be *http.Transport; proxy,
// custom dialing and redirects are disabled. Injected clients are trusted Go code.
// Timeout bounds the entire operation, including discovery and model loading.
type Config struct {
	Endpoint string
	Client   *http.Client
	Timeout  time.Duration
}

type Backend struct {
	endpoint string
	client   http.Client
	timeout  time.Duration
}

var _ core.Backend = (*Backend)(nil)

func New(c Config) (*Backend, error) {
	if c.Endpoint == "" {
		c.Endpoint = "http://127.0.0.1:11434"
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, fault(core.ErrInvalidRequest)
	}
	host := u.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fault(core.ErrInvalidRequest)
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	u.Host = net.JoinHostPort(ip.String(), port)
	u.Path = ""
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Minute
	}
	if c.Timeout < 0 {
		return nil, fault(core.ErrInvalidRequest)
	}
	client := http.Client{}
	if c.Client != nil {
		client = *c.Client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if client.Transport != nil {
		t, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil, fault(core.ErrUnsupported)
		}
		transport = t.Clone()
	}
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	// Pin every dial to the validated literal endpoint, regardless of DNS/env.
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", u.Host)
	}
	transport.Dial = nil
	transport.DialTLS = nil
	transport.DialTLSContext = nil
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 0 // model loading is covered by operation deadline
	client.Transport = transport
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Backend{endpoint: u.String(), client: client, timeout: c.Timeout}, nil
}

// CloseIdleConnections releases pooled connections; it does not cancel active jobs.
func (b *Backend) CloseIdleConnections() { b.client.CloseIdleConnections() }

func fault(code core.ErrorCode) *core.Fault {
	message := "Ollama operation failed"
	switch code {
	case core.ErrInvalidRequest:
		message = "Ollama request or context budget is invalid"
	case core.ErrUnsupported:
		message = "Ollama version, model, or option is unsupported"
	case core.ErrLimitExceeded:
		message = "Ollama response exceeds adapter limits"
	case core.ErrBackendUnavailable:
		message = "Ollama is unavailable"
	}
	return &core.Fault{Code: code, Message: message, Retryable: code == core.ErrBackendUnavailable}
}

func transportError(ctx context.Context) error {
	if ctx.Err() == context.Canceled {
		return ctx.Err()
	}
	return fault(core.ErrBackendUnavailable)
}

func (b *Backend) request(ctx context.Context, path string, body any) (*http.Response, error) {
	method := http.MethodGet
	var buf bytes.Buffer
	if body != nil {
		method = http.MethodPost
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, fault(core.ErrInvalidRequest)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint+path, &buf)
	if err != nil {
		return nil, fault(core.ErrInvalidRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, transportError(ctx)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		// Never expose or persist an upstream body, URL, or transport error.
		switch resp.StatusCode {
		case 400, 404, 422:
			return nil, fault(core.ErrInvalidRequest)
		case 429, 502, 503, 504:
			return nil, fault(core.ErrBackendUnavailable)
		default:
			return nil, fault(core.ErrBackendFailure)
		}
	}
	return resp, nil
}

func (b *Backend) json(ctx context.Context, path string, body, out any) error {
	resp, err := b.request(ctx, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, metadataLimit+1))
	if err != nil {
		return transportError(ctx)
	}
	if len(data) > metadataLimit {
		return fault(core.ErrLimitExceeded)
	}
	if !validObject(data) || json.Unmarshal(data, out) != nil {
		return fault(core.ErrBackendFailure)
	}
	return nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 512 || !utf8.ValidString(id) || strings.ContainsAny(id, " \t\r\n?#%\\") {
		return false
	}
	last := strings.ToLower(id[strings.LastIndex(id, ":")+1:])
	return last != "cloud" && last != "local" && !strings.HasSuffix(last, "-cloud")
}
