package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"taskworker.local/taskworker/internal/core"
	"testing"
)

// v0.18.3 Model.String orders SYSTEM, RENDERER, PARAMETER. Its quote helper
// returns a lone quote unchanged: no newline or leading/trailing space.
// Thus a system value `"` and stop value `"` can serialize across the renderer.
func TestReviewAmbiguousSystemHidesRenderer(t *testing.T) {
	for _, mode := range []string{"discovery", "selection", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			f := defaults()
			original := f.show.(map[string]any)
			changed := map[string]any{}
			for k, v := range original {
				changed[k] = v
			}
			changed["system"] = "\""
			changed["parameters"] = fmt.Sprintf("%-*s %#v", 30, "stop", "\"")
			changed["modelfile"] = "FROM /local/weights\nTEMPLATE {{ .Messages }}\nSYSTEM \"\nRENDERER qwen3.5\nPARAMETER stop \"\n"
			if mode == "refresh" {
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/api/show" {
						return false
					}
					show := original
					if f.shows.Add(1) > 1 {
						show = changed
					}
					json.NewEncoder(w).Encode(show)
					return true
				}
			} else {
				f.show = changed
			}
			b, _ := f.server(t)
			if mode == "discovery" {
				models, err := b.Models(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(models) != 1 || models[0].Capabilities.TextGeneration {
					t.Fatalf("renderer advertised as supported: %+v", models)
				}
				return
			}
			callbacks := 0
			_, err := b.Generate(context.Background(), input(), func(string) error { callbacks++; return nil })
			var fault *core.Fault
			if !errors.As(err, &fault) || fault.Code != core.ErrUnsupported || callbacks != 0 {
				t.Fatalf("renderer must be rejected; err=%v callbacks=%d", err, callbacks)
			}
		})
	}
}
