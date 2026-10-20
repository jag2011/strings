package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	agent "github.com/OpenRouterTeam/go-agent"
	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/retry"

	"github.com/jag2011/strings/internal/store"
)

// OpenRouter runs sessions through OpenRouter's Responses API, which
// reaches the models of many providers with one key.
type OpenRouter struct {
	client agent.ResponseSender
}

func NewOpenRouter(apiKey string) *OpenRouter { return newOpenRouter(apiKey) }

func newOpenRouter(apiKey string, opts ...openrouter.SDKOption) *OpenRouter {
	sdk := openrouter.New(append([]openrouter.SDKOption{
		openrouter.WithSecurity(apiKey),
		openrouter.WithXTitle("strings"),
		// Server errors are retried, but not for as long as the SDK would
		// (an hour): a job that can't reach the model fails and can be
		// started again.
		openrouter.WithRetryConfig(retry.Config{
			Strategy:              "backoff",
			Backoff:               &retry.BackoffStrategy{InitialInterval: 1000, MaxInterval: 30000, Exponent: 1.5, MaxElapsedTime: 300000},
			RetryConnectionErrors: true,
		}),
		// Not WithTimeout: the SDK cancels that context when Send returns,
		// before a streamed answer is read. The client's timeout covers the
		// whole response.
		openrouter.WithClient(&http.Client{Timeout: 10 * time.Minute}),
	}, opts...)...)
	return &OpenRouter{client: agent.SDKClient{Client: sdk}}
}

// maxSessionSteps is far above what a session takes; sessions end through
// BeforeStep.
const maxSessionSteps = 1000

func (o *OpenRouter) Run(ctx context.Context, s Session) (string, error) {
	tools := make([]agent.Tool, len(s.Tools))
	for i, t := range s.Tools {
		tools[i] = t.sdkTool()
	}
	res, err := agent.CallModel(ctx, o.client, agent.CallModelInput{
		Model:                  s.Model,
		Input:                  s.Prompt,
		AdditionalInstructions: s.System,
		Tools:                  tools,
		MaxTurns:               maxSessionSteps,
		// Ending a session through BeforeStep needs no extra answer.
		AllowFinalResponse: false,
		// No unreported extra call when the last answer is empty.
		StrictFinalResponse: true,
		Request: components.ResponsesRequest{
			// Anthropic models cache only with this; it puts the breakpoint
			// on the last block, so each call reads the conversation so far
			// from the cache. Other providers cache on their own.
			CacheControl: &components.AnthropicCacheControlDirective{Type: components.AnthropicCacheControlDirectiveTypeEphemeral},
		},
		BeforeTurn: func(ctx context.Context, _ agent.TurnContext) error {
			if s.BeforeStep == nil {
				return nil
			}
			return s.BeforeStep(ctx)
		},
		AfterTurn: func(ctx context.Context, _ agent.TurnContext, step agent.StepResult) error {
			if s.AfterStep != nil {
				s.AfterStep(ctx, usage(step.Usage))
			}
			if msg := oddAnswer(step); msg != "" && s.Log != nil {
				s.Log(ctx, msg)
			}
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	return res.Text(ctx)
}

func usage(u *components.Usage) store.AIUsage {
	out := store.AIUsage{Steps: 1}
	if u == nil {
		return out
	}
	out.InputTokens, out.OutputTokens = u.InputTokens, u.OutputTokens
	out.CachedTokens = u.InputTokensDetails.CachedTokens
	if c, ok := u.Cost.GetOrZero(); ok {
		out.Cost = c
	}
	return out
}

// oddAnswer describes a model answer the session can't go on from, or one
// with a tool call that go-agent drops: unless arguments parse as JSON, the
// call is skipped silently, and an answer without calls ends the session.
// It returns "" for ordinary answers.
func oddAnswer(step agent.StepResult) string {
	resp := step.Response
	var items []string
	broken := false
	for _, it := range resp.Output {
		switch {
		case it.OutputFunctionCallItem != nil:
			fc := it.OutputFunctionCallItem
			if err := json.Unmarshal([]byte(fc.Arguments), new(any)); err != nil && strings.TrimSpace(fc.Arguments) != "" {
				items = append(items, fmt.Sprintf("%s call with broken arguments (%d bytes: %v)", fc.Name, len(fc.Arguments), err))
				broken = true
			} else {
				items = append(items, fc.Name+" call")
			}
		case it.OutputMessageItem != nil:
			items = append(items, "text")
		case it.OutputReasoningItem != nil:
			items = append(items, "reasoning")
		default:
			items = append(items, string(it.Type))
		}
	}
	status := string(resp.Status)
	if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != nil {
		status += " (" + string(*resp.IncompleteDetails.Reason) + ")"
	}
	if !broken && len(step.ToolCalls) > 0 && resp.Status == components.OpenAIResponsesResponseStatusCompleted {
		return ""
	}
	if !broken && len(step.ToolCalls) == 0 && strings.TrimSpace(step.Text) != "" && resp.Status == components.OpenAIResponsesResponseStatusCompleted {
		return "" // a final answer
	}
	out := "the model answered without a usable tool call: " + status
	if len(items) == 0 {
		return out + ", no output"
	}
	return out + "; " + strings.Join(items, ", ")
}
