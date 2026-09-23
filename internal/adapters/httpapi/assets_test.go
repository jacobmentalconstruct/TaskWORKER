package httpapi_test

import (
	"net/http/httptest"
	"strings"
	"taskworker.local/taskworker/internal/adapters/httpapi"
	"testing"
)

func TestEmbeddedAssets(t *testing.T) {
	for _, host := range []string{"127.0.0.1:7433", "127.0.0.1:80", "[::1]:80"} {
		h := httpapi.NewHandler(nil, host)
		for path, media := range map[string]string{"/": "text/html; charset=utf-8", "/app.js": "text/javascript; charset=utf-8", "/state.js": "text/javascript; charset=utf-8", "/app.css": "text/css; charset=utf-8"} {
			for _, authority := range []string{host, strings.TrimSuffix(host, ":80")} {
				r := httptest.NewRequest("GET", "http://"+authority+path, nil)
				r.Header.Set("Origin", "http://"+authority)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 || w.Header().Get("Content-Type") != media || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") || w.Body.Len() == 0 {
					t.Fatal(path, w.Code, w.Header())
				}
			}
		}
		for _, tc := range []struct {
			method, path, origin string
			want                 int
		}{{"POST", "/", "", 405}, {"GET", "/?x=1", "", 400}, {"GET", "/app.js", "http://evil.example", 403}, {"GET", "/.dev-logs/CURRENT_STATE.md", "", 404}, {"GET", "/../go.mod", "", 404}, {"GET", "/assets/", "", 404}, {"GET", "/%61pp.js", "", 400}, {"GET", "/%2e%2e/go.mod", "", 400}, {"GET", "/missing", "", 404}, {"GET", "/state.test.mjs", "", 404}} {
			r := httptest.NewRequest(tc.method, "http://"+host+tc.path, nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(tc, w.Code)
			}
		}
	}
}
