package httpapi_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/core"
)

type authorityService struct {
	core.Service
	mutations, watches int
}

func (s *authorityService) SetQueuePaused(context.Context, bool) (core.QueueState, error) {
	s.mutations++
	return core.QueueState{Paused: true}, nil
}
func (s *authorityService) Watch(context.Context, core.Cursor) (core.EventStream, error) {
	s.watches++
	return authorityStream{}, nil
}

type authorityStream struct{}

func (authorityStream) Next(context.Context) (core.Event, error) { return core.Event{}, io.EOF }
func (authorityStream) Close() error                             { return nil }

func TestAuthorityDefaultPortEquivalence(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1", "http://127.0.0.1:80", "http://[::1]", "http://[::1]:80", "http://127.0.0.1:7433", "http://[::1]:7433"} {
		t.Run(endpoint, func(t *testing.T) {
			u, e := httpapi.Endpoint(endpoint)
			if e != nil {
				t.Fatal(e)
			}
			hosts := []string{u.Host}
			if u.Port() == "80" {
				hosts = append(hosts, strings.TrimSuffix(u.Host, ":80"))
			}
			origins := []string{""}
			for _, host := range hosts {
				origins = append(origins, "http://"+host)
			}
			for _, host := range hosts {
				for _, origin := range origins {
					for _, route := range []string{"health", "mutation", "sse"} {
						t.Run(host+"_"+origin+"_"+route, func(t *testing.T) {
							s := &authorityService{}
							h := httpapi.NewHandler(s, u.Host)
							method, path, body := "GET", "/v1/health", ""
							if route == "mutation" {
								method, path, body = "POST", "/v1/queue/pause", "{}"
							}
							if route == "sse" {
								path = "/v1/events?after=" + strings.Repeat("a", 32) + ":0"
							}
							r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
							if origin != "" {
								r.Header.Set("Origin", origin)
							}
							if body != "" {
								r.Header.Set("Content-Type", "application/json")
							}
							w := httptest.NewRecorder()
							h.ServeHTTP(w, r)
							if w.Code != 200 {
								t.Fatal(w.Code, w.Body.String())
							}
							if route == "mutation" && s.mutations != 1 {
								t.Fatal("mutation not called")
							}
							if route == "sse" && (s.watches != 1 || w.Header().Get("Content-Type") != "text/event-stream") {
								t.Fatal("watch not called")
							}
						})
					}
				}
			}
		})
	}
}

func TestAuthorityPolicyStillRejectsOtherSites(t *testing.T) {
	for _, socket := range []string{"127.0.0.1:80", "[::1]:80", "127.0.0.1:7433", "[::1]:7433"} {
		t.Run(socket, func(t *testing.T) {
			base := strings.TrimSuffix(strings.TrimSuffix(socket, ":80"), ":7433")
			hosts := []string{"evil.example", "localhost", "127.0.0.2", "[::2]", base + ":81", base + ":080", base + ":", base + ".", base + "/", base + "@evil.example"}
			other := "[::1]"
			if strings.HasPrefix(socket, "[") {
				other = "127.0.0.1"
			}
			hosts = append(hosts, other, other+":80", other+":7433")
			if strings.HasSuffix(socket, ":7433") {
				hosts = append(hosts, base, base+":80")
			} else {
				hosts = append(hosts, base+":7433")
			}
			for _, host := range hosts {
				for _, route := range []string{"health", "mutation", "sse"} {
					t.Run("host_"+host+"_"+route, func(t *testing.T) { checkAuthorityRejected(t, socket, host, nil, "", route) })
				}
			}
			origins := []string{"", "null", "https://" + socket, "http://" + socket + "/", "http://" + socket + "?x", "http://" + socket + "#x", "http://user@" + socket, "HTTP://" + socket, "http://" + socket + " http://evil.example"}
			for _, host := range hosts {
				origins = append(origins, "http://"+host)
			}
			for _, origin := range origins {
				for _, route := range []string{"health", "mutation", "sse"} {
					t.Run("origin_"+origin+"_"+route, func(t *testing.T) { checkAuthorityRejected(t, socket, socket, []string{origin}, "", route) })
				}
			}
			for _, route := range []string{"health", "mutation", "sse"} {
				checkAuthorityRejected(t, socket, socket, []string{"http://" + socket, "http://" + socket}, "", route)
				checkAuthorityRejected(t, socket, socket, []string{"http://" + socket}, "cross-site", route)
			}
		})
	}
}
func checkAuthorityRejected(t *testing.T, socket, host string, origins []string, fetch, route string) {
	t.Helper()
	s := &authorityService{}
	h := httpapi.NewHandler(s, socket)
	method, path, body := "GET", "/v1/health", ""
	if route == "mutation" {
		method, path, body = "POST", "/v1/queue/pause", "{}"
	}
	if route == "sse" {
		path = "/v1/events?after=" + strings.Repeat("a", 32) + ":0"
	}
	r := httptest.NewRequest(method, "http://"+socket+path, strings.NewReader(body))
	r.Host = host
	if origins != nil {
		r.Header["Origin"] = origins
	}
	if fetch != "" {
		r.Header.Set("Sec-Fetch-Site", fetch)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || s.mutations != 0 || s.watches != 0 {
		t.Fatalf("status %d; mutations %d watches %d", w.Code, s.mutations, s.watches)
	}
}
