// Package webui contains only the product's embedded browser assets.
package webui

import _ "embed"

//go:embed index.html
var page []byte

//go:embed app.css
var style []byte

//go:embed app.js
var app []byte

//go:embed state.js
var state []byte

// Asset is an exact allowlist, never a filesystem or SPA fallback.
func Asset(path string) ([]byte, string, bool) {
	switch path {
	case "/":
		return page, "text/html; charset=utf-8", true
	case "/app.css":
		return style, "text/css; charset=utf-8", true
	case "/app.js":
		return app, "text/javascript; charset=utf-8", true
	case "/state.js":
		return state, "text/javascript; charset=utf-8", true
	}
	return nil, "", false
}
