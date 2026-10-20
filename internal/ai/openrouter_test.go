package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"

	"github.com/jag2011/strings/internal/store"
)

// fakeResponses answers Responses API calls with canned outputs, in order,
// and keeps the request bodies.
type fakeResponses struct {
	mu       sync.Mutex
	outputs  []string // JSON arrays of output items
	requests []map[string]any
}

func (f *fakeResponses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	f.mu.Lock()
	n := len(f.requests)
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if n >= len(f.outputs) {
		http.Error(w, `{"error": {"message": "no more outputs"}}`, http.StatusBadRequest)
		return
	}
	if f.outputs[n] == breakOff {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	// Streamed like the real API: the body is read after the SDK's Send
	// returned, which a plain JSON answer wouldn't test.
	event := []byte(`{"type": "response.completed", "sequence_number": 0, "response": {"id": "resp_` + string(rune('a'+n)) + `", "object": "response", "created_at": 1, "completed_at": 2, "model": "test/model",
		"status": "completed", "output": ` + f.outputs[n] + `, "error": null, "incomplete_details": null, "instructions": null,
		"metadata": {}, "parallel_tool_calls": true, "temperature": null, "top_p": null, "frequency_penalty": null,
		"presence_penalty": null, "tool_choice": "auto", "tools": [],
		"usage": {"input_tokens": 1200, "input_tokens_details": {"cached_tokens": 1000}, "output_tokens": 30,
			"output_tokens_details": {"reasoning_tokens": 0}, "total_tokens": 1230, "cost": 0.0125}}}`)
	var line bytes.Buffer // an SSE data line can't span lines
	json.Compact(&line, event)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", line.Bytes())
}

const (
	breakOff   = "break off" // a stream that ends without a response
	callGetJob = `[{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_job", "arguments": "{}", "status": "completed"}]`
	sayDone    = `[{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
		"content": [{"type": "output_text", "text": "Done.", "annotations": []}]}]`
)

func testSession(steps *[]store.AIUsage) Session {
	return Session{
		Model:  "test/model",
		System: "You translate.",
		Prompt: "Start the job.",
		Tools: []Tool{{
			Name:        "get_job",
			Description: "Returns the job.",
			Schema:      object(map[string]any{}),
			Call: func(context.Context, json.RawMessage) (any, error) {
				return map[string]any{"strings_left": 3}, nil
			},
		}},
		BeforeStep: func(ctx context.Context) error { return nil },
		AfterStep:  func(_ context.Context, u store.AIUsage) { *steps = append(*steps, u) },
	}
}

func TestOpenRouterSession(t *testing.T) {
	fake := &fakeResponses{outputs: []string{callGetJob, sayDone}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	o := newOpenRouter("key", openrouter.WithServerURL(srv.URL))

	var steps []store.AIUsage
	text, err := o.Run(context.Background(), testSession(&steps))
	if err != nil {
		t.Fatal(err)
	}
	if text != "Done." || len(fake.requests) != 2 {
		t.Fatalf("text %q after %d requests", text, len(fake.requests))
	}
	if len(steps) != 2 || steps[0] != (store.AIUsage{Steps: 1, InputTokens: 1200, CachedTokens: 1000, OutputTokens: 30, Cost: 0.0125}) {
		t.Errorf("usage: %+v", steps)
	}

	req := fake.requests[0]
	if req["model"] != "test/model" || req["instructions"] != "You translate." {
		t.Errorf("request: %v", req)
	}
	if cc, _ := req["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("cache_control: %v", req["cache_control"])
	}
	tool := req["tools"].([]any)[0].(map[string]any)
	params := tool["parameters"].(map[string]any)
	if tool["name"] != "get_job" || tool["strict"] != true || params["additionalProperties"] != false || params["required"] == nil {
		t.Errorf("tool: %v", tool)
	}
	// The second call carries the tool's result.
	in, _ := json.Marshal(fake.requests[1]["input"])
	if !strings.Contains(string(in), `"function_call_output"`) || !strings.Contains(string(in), `strings_left`) {
		t.Errorf("second input: %s", in)
	}
}

// A tool call whose arguments aren't JSON would be dropped by go-agent,
// ending the session. Instead the model gets an error and tries again; a
// stream that breaks off is sent again.
func TestOpenRouterRecovers(t *testing.T) {
	broken := `[{"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "get_job", "arguments": "{\"translations\": [{\"key\": \"a", "status": "completed"}]`
	fake := &fakeResponses{outputs: []string{callGetJob, broken, breakOff, sayDone}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	o := newOpenRouter("key", openrouter.WithServerURL(srv.URL))

	var steps []store.AIUsage
	var logged []string
	s := testSession(&steps)
	s.Log = func(_ context.Context, msg string) { logged = append(logged, msg) }
	text, err := o.Run(context.Background(), s)
	if err != nil || text != "Done." || len(fake.requests) != 4 || len(steps) != 3 {
		t.Fatalf("text %q, err %v after %d requests, %d steps", text, err, len(fake.requests), len(steps))
	}
	want := `get_job call with arguments that aren't valid JSON (unexpected end of JSON input at byte 28 of 28, after "{\"translations\": [{\"key\": \"a"); asked the model to send it again`
	if len(logged) != 1 || logged[0] != want {
		t.Errorf("logged: %q", logged)
	}
	in, _ := json.Marshal(fake.requests[3]["input"])
	if !strings.Contains(string(in), "weren't valid JSON, so nothing happened") {
		t.Errorf("the model wasn't told: %s", in)
	}
}

func TestOpenRouterBrokenStream(t *testing.T) {
	fake := &fakeResponses{outputs: []string{breakOff, breakOff, breakOff}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	o := newOpenRouter("key", openrouter.WithServerURL(srv.URL))
	var steps []store.AIUsage
	if _, err := o.Run(context.Background(), testSession(&steps)); err == nil || !strings.Contains(err.Error(), "broke off") || len(fake.requests) != 3 {
		t.Fatalf("err %v after %d requests", err, len(fake.requests))
	}
}

func TestOpenRouterBeforeStepEndsSession(t *testing.T) {
	fake := &fakeResponses{outputs: []string{callGetJob, sayDone}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	o := newOpenRouter("key", openrouter.WithServerURL(srv.URL))

	var steps []store.AIUsage
	s := testSession(&steps)
	s.BeforeStep = func(context.Context) error {
		if len(steps) == 1 {
			return errSessionFull
		}
		return nil
	}
	if _, err := o.Run(context.Background(), s); !errors.Is(err, errSessionFull) {
		t.Fatalf("err: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("%d requests", len(fake.requests))
	}
}

// TestOpenRouterLive calls the real API when OPENROUTER_API_KEY is set, with
// OPENROUTER_TEST_MODEL or a cheap default: tools, usage, cost and caching.
func TestOpenRouterLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	model := os.Getenv("OPENROUTER_TEST_MODEL")
	if model == "" {
		model = "anthropic/claude-haiku-5.5"
	}
	var steps []store.AIUsage
	s := testSession(&steps)
	s.Model = model
	// Long enough to be cached by every provider.
	s.System = "Call get_job once, then answer with the number of strings left and nothing else.\n" + strings.Repeat("Translate carefully. ", 1500)
	for i := range 2 {
		text, err := NewOpenRouter(key).Run(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("run %d: %q", i+1, text)
		if !strings.Contains(text, "3") {
			t.Errorf("answer %q doesn't use the tool result", text)
		}
	}
	var cost float64
	var cached int64
	for i, u := range steps {
		t.Logf("step %d: %+v", i+1, u)
		cost += u.Cost
		cached += u.CachedTokens
	}
	if cost == 0 {
		t.Error("no cost reported")
	}
	if cached == 0 {
		t.Error("nothing read from the cache")
	}
}
