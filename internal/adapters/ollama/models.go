package ollama

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"taskworker.local/taskworker/internal/core"
)

// This is deliberately an audited version allowlist, not a guessed minimum.
const supportedVersion = "0.18.3"

type installed struct {
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	RemoteHost  string `json:"remote_host"`
	RemoteModel string `json:"remote_model"`
}

type details struct {
	Capabilities []string          `json:"capabilities"`
	Template     string            `json:"template"`
	Modelfile    string            `json:"modelfile"`
	System       string            `json:"system"`
	Renderer     string            `json:"renderer"`
	Messages     []json.RawMessage `json:"messages"`
	RemoteHost   string            `json:"remote_host"`
	RemoteModel  string            `json:"remote_model"`
	Details      struct {
		Format string `json:"format"`
	} `json:"details"`
	Info map[string]json.RawMessage `json:"model_info"`
}

func (b *Backend) version(ctx context.Context) error {
	var v struct {
		Version string `json:"version"`
	}
	if err := b.json(ctx, "/api/version", nil, &v); err != nil {
		return err
	}
	if v.Version != supportedVersion {
		return fault(core.ErrUnsupported)
	}
	return nil
}

func (b *Backend) inventory(ctx context.Context) ([]installed, error) {
	var v struct {
		Models []installed `json:"models"`
	}
	if err := b.json(ctx, "/api/tags", nil, &v); err != nil {
		return nil, err
	}
	if len(v.Models) > 1024 {
		return nil, fault(core.ErrLimitExceeded)
	}
	seen := map[string]bool{}
	for _, m := range v.Models {
		if m.Name == "" || len(m.Name) > 512 || seen[m.Name] {
			return nil, fault(core.ErrBackendFailure)
		}
		seen[m.Name] = true
	}
	return v.Models, nil
}

func (b *Backend) show(ctx context.Context, id string) (details, core.ContextInfo, error) {
	var d details
	var info core.ContextInfo
	if err := b.json(ctx, "/api/show", map[string]string{"model": id + ":local"}, &d); err != nil {
		return d, info, err
	}
	var arch string
	_ = json.Unmarshal(d.Info["general.architecture"], &arch)
	var capacity int
	if arch != "" && len(arch) <= 128 && json.Unmarshal(d.Info[arch+".context_length"], &capacity) == nil && capacity > 0 {
		now := time.Now().UTC()
		info.ModelCapacityTokens = &capacity
		info.ModelCapacitySource = "ollama /api/show model_info." + arch + ".context_length"
		info.ModelCapacityObservedAt = &now
	}
	return d, info, nil
}

func usable(m installed, d details) bool {
	return validID(m.Name) && m.RemoteHost == "" && m.RemoteModel == "" &&
		d.RemoteHost == "" && d.RemoteModel == "" && d.Details.Format == "gguf" &&
		len(d.Messages) == 0 && d.Renderer == "" && d.Template != "" && templateOnlyModelfile(d.Modelfile, d.Template, d.System) &&
		slices.Contains(d.Capabilities, "completion") && !slices.Contains(d.Capabilities, "vision") && !slices.Contains(d.Capabilities, "image")
}

func (b *Backend) loaded(ctx context.Context, m installed) (*core.ContextObservation, error) {
	var v struct {
		Models []struct {
			Name    string `json:"name"`
			Digest  string `json:"digest"`
			Context *int   `json:"context_length"`
		} `json:"models"`
	}
	if err := b.json(ctx, "/api/ps", nil, &v); err != nil {
		return nil, err
	}
	for _, p := range v.Models {
		if p.Name == m.Name && p.Digest == m.Digest && p.Context != nil && *p.Context > 0 {
			return &core.ContextObservation{Tokens: *p.Context, Model: m.Name, Source: "ollama /api/ps context_length (shared residency observation)", ObservedAt: time.Now().UTC()}, nil
		}
	}
	return nil, nil
}

func (b *Backend) Models(ctx context.Context) ([]core.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	if err := b.version(ctx); err != nil {
		return nil, err
	}
	models, err := b.inventory(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.Model, 0, len(models))
	for _, m := range models {
		model := core.Model{ID: m.Name}
		// No show on cloud identifiers: show itself may contact an upstream server.
		if validID(m.Name) && m.RemoteHost == "" && m.RemoteModel == "" {
			d, info, err := b.show(ctx, m.Name)
			if err != nil {
				return nil, err
			}
			model.Context = info
			model.Context.Loaded, err = b.loaded(ctx, m)
			if err != nil {
				return nil, err
			}
			if usable(m, d) {
				model.Capabilities = core.Capabilities{TextGeneration: true, Streaming: true, Cancellation: true, ContextRequest: true, LoadedContext: true}
			}
		}
		out = append(out, model)
	}
	return out, nil
}

func (b *Backend) selectModel(ctx context.Context, id string) (installed, details, core.ContextInfo, error) {
	var empty installed
	if !validID(id) {
		return empty, details{}, core.ContextInfo{}, fault(core.ErrUnsupported)
	}
	models, err := b.inventory(ctx)
	if err != nil {
		return empty, details{}, core.ContextInfo{}, err
	}
	for _, m := range models {
		if m.Name != id {
			continue
		}
		if m.RemoteHost != "" || m.RemoteModel != "" {
			return empty, details{}, core.ContextInfo{}, fault(core.ErrUnsupported)
		}
		d, info, err := b.show(ctx, id)
		if err == nil && !usable(m, d) {
			err = fault(core.ErrUnsupported)
		}
		return m, d, info, err
	}
	return empty, details{}, core.ContextInfo{}, fault(core.ErrInvalidRequest)
}
