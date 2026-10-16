package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const modelsJSON = `{"data": [
	{"id": "openai/gpt-5.5", "name": "OpenAI: GPT-5.5", "architecture": {"output_modalities": ["text"]},
		"pricing": {"prompt": "0.000005", "completion": "0.00003"}, "supported_parameters": ["tools", "temperature"]},
	{"id": "anthropic/claude-opus-5.5", "name": "Anthropic: Claude Opus 5.5", "architecture": {"output_modalities": ["text"]},
		"pricing": {"prompt": "0.000004", "completion": "0.00002"}, "supported_parameters": ["tools"]},
	{"id": "anthropic/claude-opus-5.5:batch", "name": "batch", "architecture": {"output_modalities": ["text"]},
		"pricing": {"prompt": "0.000002", "completion": "0.00001"}, "supported_parameters": ["tools"]},
	{"id": "moondream/moondream3", "name": "no tools", "architecture": {"output_modalities": ["text"]},
		"pricing": {"prompt": "0", "completion": "0"}, "supported_parameters": ["temperature"]},
	{"id": "openai/gpt-5-image", "name": "images", "architecture": {"output_modalities": ["image", "text"]},
		"pricing": {"prompt": "0.00001", "completion": "0.00001"}, "supported_parameters": ["tools"]}
]}`

func TestCatalog(t *testing.T) {
	var calls, fail atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() == 1 {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		io.WriteString(w, modelsJSON)
	}))
	defer srv.Close()
	c := NewCatalog()
	c.URL = srv.URL
	ctx := context.Background()

	fail.Store(1)
	if ms := c.Models(ctx); ms != nil {
		t.Fatalf("while down: %v", ms)
	}
	fail.Store(0)
	ms := c.Models(ctx)
	if len(ms) != 2 || ms[0].ID != "anthropic/claude-opus-5.5" || ms[1].ID != "openai/gpt-5.5" {
		t.Fatalf("models: %+v", ms)
	}
	if ms[0].Input != 4 || ms[0].Output != 20 || ms[0].Label() != "Anthropic: Claude Opus 5.5 · $4 / $20 per M tokens" {
		t.Errorf("price: %+v %q", ms[0], ms[0].Label())
	}
	if c.Models(ctx); calls.Load() != 2 {
		t.Errorf("not cached: %d calls", calls.Load())
	}

	// Stale: the cached list comes back at once, refreshed in the background,
	// and kept if the refresh fails.
	c.TTL = 0
	fail.Store(1)
	if ms := c.Models(ctx); len(ms) != 2 {
		t.Errorf("stale list: %v", ms)
	}
	for range 100 {
		c.mu.Lock()
		done := !c.fetching
		c.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 3 || len(c.Models(ctx)) != 2 {
		t.Errorf("refresh: %d calls", calls.Load())
	}
}
