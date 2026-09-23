package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"taskworker.local/taskworker/internal/core"
)

type Client struct {
	endpoint string
	http     *http.Client
}

func NewClient(endpoint string) (*Client, error) {
	u, err := Endpoint(endpoint)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableCompression = true
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "tcp", u.Host) }
	tr.ResponseHeaderTimeout = 35 * time.Second
	return &Client{endpoint: u.String(), http: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }

const connectionMessage = "cannot reach service; start serve or check --server; create acceptance may be uncertain"

func connection() error {
	return &core.Fault{Code: core.ErrUnavailable, Message: connectionMessage, Retryable: true}
}

// Messages of faults this client raises itself after a request was sent, when a
// reply cannot be read. Service faults are rewritten by PublicFault to the bare
// code, so a service can never forge them.
const (
	MessageResponseTooLarge = "service response too large"
	MessageInvalidResponse  = "invalid service response"
)

// IsUnreadableResponse reports whether err is this client's own failure to read
// a reply (oversized or undecodable). After a create was sent it cannot prove
// rejection, exactly like a connection failure.
func IsUnreadableResponse(err error) bool {
	var f *core.Fault
	if !errors.As(err, &f) || f == nil {
		return false
	}
	return (f.Code == core.ErrLimitExceeded && f.Message == MessageResponseTooLarge) || (f.Code == core.ErrInternal && f.Message == MessageInvalidResponse)
}

// IsConnectionFault reports whether err is this client's own transport failure
// (no usable response), as opposed to a Fault the service itself returned.
// Service faults are rewritten by PublicFault and can never carry this message.
func IsConnectionFault(err error) bool {
	var f *core.Fault
	return errors.As(err, &f) && f != nil && f.Code == core.ErrUnavailable && f.Message == connectionMessage
}
func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if e != nil {
		return nil, invalid()
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return nil, connection()
	}
	return resp, nil
}
func (c *Client) Call(ctx context.Context, method, path string, command any, dst any) error {
	var b []byte
	var err error
	if command != nil {
		b, err = json.Marshal(command)
		if err != nil {
			return invalid()
		}
	}
	resp, err := c.request(ctx, method, path, b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limit := int64(MaxResponse)
	if resp.StatusCode != 200 {
		limit = 4096
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return connection()
	}
	if int64(len(data)) > limit {
		return &core.Fault{Code: core.ErrLimitExceeded, Message: MessageResponseTooLarge}
	}
	if resp.StatusCode != 200 {
		var f core.Fault
		if json.Unmarshal(data, &f) != nil {
			return connection()
		}
		return PublicFault(&f)
	}
	if json.Unmarshal(data, dst) != nil {
		return &core.Fault{Code: core.ErrInternal, Message: MessageInvalidResponse}
	}
	return nil
}
func (c *Client) Get(ctx context.Context, id core.JobID) (core.Job, error) {
	var j core.Job
	e := c.Call(ctx, "GET", "/v1/jobs/"+url.PathEscape(string(id)), nil, &j)
	return j, e
}

// Wait observes only. A cancelled wait never calls Cancel or resubmits a command.
func (c *Client) Wait(ctx context.Context, id core.JobID) (core.Job, error) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		j, e := c.Get(ctx, id)
		if e != nil {
			return j, e
		}
		if core.Terminal(j.State) {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, connection()
		case <-tick.C:
		}
	}
}

type Frame struct {
	Kind string          `json:"kind"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data"`
}

// Watch emits only complete frames. Callers advance the cursor after application;
// errors/disconnects return explicitly, allowing reconnect with that cursor.
func (c *Client) Watch(ctx context.Context, after string, apply func(Frame) error) error {
	path := "/v1/events"
	if after != "" {
		if _, e := ParseCursor(after); e != nil {
			return e
		}
		path += "?after=" + url.QueryEscape(after)
	}
	resp, e := c.request(ctx, "GET", path, nil)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var f core.Fault
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&f) != nil {
			return connection()
		}
		return PublicFault(&f)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		return connection()
	}
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 4096), MaxResponse+1024)
	var f Frame
	var size int
	for scan.Scan() {
		line := scan.Text()
		size += len(line)
		if size > MaxResponse+1024 {
			return &core.Fault{Code: core.ErrLimitExceeded, Message: "event frame too large"}
		}
		if line == "" {
			if f.Kind != "" {
				if f.Kind == "error" {
					var fault core.Fault
					if json.Unmarshal(f.Data, &fault) != nil {
						return connection()
					}
					return PublicFault(&fault)
				}
				if f.Kind != "snapshot" && f.Kind != "event" {
					return connection()
				}
				cur, e := ParseCursor(f.ID)
				if e != nil {
					return e
				}
				var envelope struct {
					Cursor core.Cursor `json:"cursor"`
				}
				if json.Unmarshal(f.Data, &envelope) != nil || envelope.Cursor != cur {
					return connection()
				}
				if f.Kind == "event" && len(f.Data) > MaxEvent {
					return connection()
				}
				if e := apply(f); e != nil {
					return e
				}
			}
			f = Frame{}
			size = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			f.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			f.Kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if f.Data != nil {
				return connection()
			}
			f.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		default:
			return connection()
		}
	}
	return connection()
}
