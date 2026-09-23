package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskworker.local/taskworker/internal/core"
)

// Independent source-backed fixture serialization, including quote()'s loss of
// distinction between literal quotes and delimiters. This is test data only.
func serializedValue(s string) string {
	if strings.Contains(s, "\n") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		if strings.Contains(s, `"`) {
			return `"""` + s + `"""`
		}
		return `"` + s + `"`
	}
	return s
}

func TestRendererCannotHideInGeneratedPrefix(t *testing.T) {
	// Cover every field emitted before RENDERER, including the unavailable path
	// metadata. Literal closing quotes in later stop options expose swallowing.
	values := []string{`"`, `""`, `"""`, `"text`, "first\nlast", "first \"quote\"\nlast", "first\n\"\"\"\nSYSTEM \"", "\nRENDERER text\nPARAMETER stop "}
	for _, field := range []string{"FROM", "ADAPTER", "TEMPLATE", "SYSTEM"} {
		for i, value := range values {
			t.Run(fmt.Sprintf("%s/%d", field, i), func(t *testing.T) {
				template, system := "{{ .Messages }}", ""
				path := "/local/weights"
				adapter := ""
				if field == "FROM" {
					path = value
				}
				if field == "ADAPTER" {
					adapter = "ADAPTER " + serializedValue(value) + "\n"
				}
				if field == "TEMPLATE" {
					template = value
				}
				if field == "SYSTEM" {
					system = value
				}
				source := "FROM " + path + "\n" + adapter + "TEMPLATE " + serializedValue(template) + "\n"
				if system != "" {
					source += "SYSTEM " + serializedValue(system) + "\n"
				}
				source += "RENDERER qwen3.5\nPARAMETER stop \"\nPARAMETER stop \"\"\"\n"
				if templateOnlyModelfile(source, template, system) {
					t.Fatal("real renderer hidden by generated prefix")
				}
			})
		}
	}
}

func TestSystemProvenanceAndOrdering(t *testing.T) {
	const base = "FROM /local/weights\nTEMPLATE {{ .Messages }}\n"
	for _, tc := range []struct {
		name, source, system string
		want                 bool
	}{
		{"ambiguous_lone_quote", base + "SYSTEM \"\nRENDERER qwen3.5\nPARAMETER stop \"\n", `"`, false},
		{"same_bytes_legitimate_system", base + "SYSTEM \"\nRENDERER qwen3.5\nPARAMETER stop \"\n", "\nRENDERER qwen3.5\nPARAMETER stop ", true},
		{"structured_system_missing", base, "declared elsewhere", false},
		{"system_mismatch", base + "SYSTEM actual\n", "different", false},
		{"unexpected_system", base + "SYSTEM actual\n", "", false},
		{"duplicate_system", base + "SYSTEM actual\nSYSTEM actual\n", "actual", false},
		{"system_after_tail", base + "PARAMETER seed 1\nSYSTEM actual\n", "actual", false},
		{"system_before_template", "FROM /local/weights\nSYSTEM actual\nTEMPLATE {{ .Messages }}\n", "actual", false},
		{"license_before_template", "FROM /local/weights\nLICENSE \"\nTEMPLATE fake\nRENDERER hidden\nPARAMETER stop \"\nTEMPLATE {{ .Messages }}\n", "", false},
		{"adapter_after_template", base + "ADAPTER /local/adapter\n", "", false},
		{"adapter_single_line", "FROM /local/weights\nADAPTER /local/adapter path\nTEMPLATE {{ .Messages }}\n", "", true},
		{"quoted_path", "FROM \"/local/weights\"\nTEMPLATE {{ .Messages }}\n", "", false},
		{"quoted_adapter", "FROM /local/weights\nADAPTER \"/local/adapter\"\nTEMPLATE {{ .Messages }}\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := templateOnlyModelfile(tc.source, "{{ .Messages }}", tc.system); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestAmbiguityGateAcrossBoundaries(t *testing.T) {
	for _, kind := range []string{"system_mismatch", "system_missing", "path_quote", "adapter_quote", "template_quote", "legitimate_system"} {
		for _, boundary := range []string{"discovery", "selection", "refresh"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				f := defaults()
				original := f.show.(map[string]any)
				changed := map[string]any{}
				for k, v := range original {
					changed[k] = v
				}
				const suffix = "RENDERER qwen3.5\nPARAMETER stop \"\n"
				switch kind {
				case "system_mismatch", "system_missing", "legitimate_system":
					changed["modelfile"] = original["modelfile"].(string) + "SYSTEM \"\n" + suffix
					if kind == "system_mismatch" {
						changed["system"] = `"`
					}
					if kind == "legitimate_system" {
						changed["system"] = "\nRENDERER qwen3.5\nPARAMETER stop "
					}
				case "path_quote":
					changed["modelfile"] = "FROM \"\n" + suffix + original["modelfile"].(string)
				case "adapter_quote":
					changed["modelfile"] = "FROM /local/weights\nADAPTER \"\n" + suffix + "TEMPLATE {{ .Messages }}\n"
				case "template_quote":
					changed["template"] = `"`
					changed["modelfile"] = "FROM /local/weights\nTEMPLATE \"\n" + suffix
				}
				if boundary == "refresh" {
					f.hook = func(w http.ResponseWriter, r *http.Request) bool {
						if r.URL.Path != "/api/show" {
							return false
						}
						v := original
						if f.shows.Add(1) > 1 {
							v = changed
						}
						json.NewEncoder(w).Encode(v)
						return true
					}
				} else {
					f.show = changed
				}
				b, _ := f.server(t)
				want := kind == "legitimate_system"
				if boundary == "discovery" {
					models, err := b.Models(context.Background())
					if err != nil || len(models) != 1 || models[0].Capabilities.TextGeneration != want {
						t.Fatalf("%+v %v", models, err)
					}
					return
				}
				callbacks := 0
				_, err := b.Generate(context.Background(), input(), func(string) error { callbacks++; return nil })
				if want {
					if err != nil || callbacks != 2 {
						t.Fatalf("%v callbacks=%d", err, callbacks)
					}
					return
				}
				requireCode(t, err, core.ErrUnsupported)
				if callbacks != 0 {
					t.Fatal("callback on rejected model")
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				expectedLoads := 0
				if boundary == "refresh" {
					expectedLoads = 1
				}
				if len(f.requests) != expectedLoads {
					t.Fatal("prompt sent on rejected model")
				}
			})
		}
	}
}
