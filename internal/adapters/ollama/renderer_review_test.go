package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"taskworker.local/taskworker/internal/core"
)

// Ollama 0.18.3 emits RENDERER in modelfile, but omits top-level renderer.
// Its GetModelInfo constructs ShowResponse without assigning Renderer.
func TestReviewRendererInActualShowShape(t *testing.T) {
	for _, mode := range []string{"discovery", "generation", "postload_refresh"} {
		t.Run(mode, func(t *testing.T) {
			f := defaults()
			f.show.(map[string]any)["modelfile"] = "FROM /local/weights\nTEMPLATE \"\"\"{{ .Messages }}\"\"\"\nRENDERER qwen3.5\nPARSER qwen3.5\n"
			delete(f.show.(map[string]any), "renderer")
			if mode == "postload_refresh" {
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/api/show" {
						return false
					}
					n := f.shows.Add(1)
					show := map[string]any{}
					for k, v := range f.show.(map[string]any) {
						show[k] = v
					}
					if n == 1 {
						show["modelfile"] = "FROM /local/weights\nTEMPLATE \"\"\"{{ .Messages }}\"\"\"\n"
					}
					json.NewEncoder(w).Encode(show)
					return true
				}
			}
			b, _ := f.server(t)
			if mode == "discovery" {
				models, err := b.Models(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(models) != 1 || models[0].Capabilities.TextGeneration {
					t.Fatalf("custom renderer advertised as supported: %+v", models)
				}
				return
			}
			callbacks := 0
			_, err := b.Generate(context.Background(), input(), func(string) error { callbacks++; return nil })
			var fault *core.Fault
			if !errors.As(err, &fault) || fault.Code != core.ErrUnsupported || callbacks != 0 {
				t.Fatalf("renderer-bearing model must fail unsupported before inference; err=%v callbacks=%d", err, callbacks)
			}
		})
	}
}
