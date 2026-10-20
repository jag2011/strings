// Package ai runs AI translation jobs: an agent translates the strings of a
// language through tools that read and save them, credited to an AI user.
// One worker runs one job at a time; see Runner.
package ai

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	agent "github.com/OpenRouterTeam/go-agent"

	"github.com/jag2011/strings/internal/store"
)

// Agent runs the model loop. It is an interface so tests can script it.
type Agent interface {
	// Run calls the model with the session's tools until the model answers
	// without calling a tool, or BeforeStep returns an error. It returns the
	// model's last text, or BeforeStep's error.
	Run(ctx context.Context, s Session) (string, error)
}

// Session is one conversation with the model. A job runs several sessions
// in a row, each with a fresh context, so long jobs stay small.
type Session struct {
	Model  string
	System string
	Prompt string
	Tools  []Tool
	// BeforeStep is called before each model call. An error stops the
	// session; Run returns it.
	BeforeStep func(context.Context) error
	// AfterStep reports what each model call used.
	AfterStep func(context.Context, store.AIUsage)
	// Log notes something worth knowing about the session in the run's log,
	// like an answer the agent couldn't use.
	Log func(context.Context, string)
}

// Tool is a function the model can call.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema of the arguments, valid in strict mode
	Call        func(context.Context, json.RawMessage) (any, error)
}

func (t Tool) sdkTool() agent.Tool {
	return agent.MustNewTool(agent.ToolConfig[json.RawMessage]{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.Schema,
		Execute: func(ctx context.Context, args json.RawMessage, _ agent.ToolExecuteContext) (any, error) {
			return t.Call(ctx, args)
		},
	})
}

// object is a JSON schema for an object whose properties are all required,
// as strict mode wants. The order is fixed, since the tools are part of the
// cached prompt prefix.
func object(props map[string]any) map[string]any {
	req := append([]string{}, slices.Sorted(maps.Keys(props))...) // [], not null
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}
