package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	agent "github.com/OpenRouterTeam/go-agent"
	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
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
	return &OpenRouter{client: wholeResponses{agent.SDKClient{Client: sdk}}}
}

// maxSessionSteps is far above what a session takes; sessions end through
// BeforeStep.
const maxSessionSteps = 1000

func (o *OpenRouter) Run(ctx context.Context, s Session) (string, error) {
	tools := make([]agent.Tool, len(s.Tools))
	for i, t := range s.Tools {
		call := t.Call
		t.Call = func(ctx context.Context, raw json.RawMessage) (any, error) {
			var b struct {
				Broken *string `json:"_broken_arguments"`
			}
			if json.Unmarshal(raw, &b) != nil || b.Broken == nil {
				return call(ctx, raw)
			}
			if s.Log != nil {
				s.Log(ctx, fmt.Sprintf("%s call with arguments that aren't valid JSON (%s); asked the model to send it again", t.Name, *b.Broken))
			}
			return map[string]any{"error": "Your arguments weren't valid JSON, so nothing happened: " + *b.Broken +
				". Escape double quotes and backslashes inside texts, and send the call again."}, nil
		}
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

// oddAnswer describes a model answer the session can't go on from: no
// tool call and no text, or one that is incomplete. It returns "" for
// ordinary answers.
func oddAnswer(step agent.StepResult) string {
	resp := step.Response
	completed := resp.Status == components.OpenAIResponsesResponseStatusCompleted
	if completed && (len(step.ToolCalls) > 0 || strings.TrimSpace(step.Text) != "") {
		return ""
	}
	var items []string
	for _, it := range resp.Output {
		switch {
		case it.OutputFunctionCallItem != nil:
			items = append(items, it.OutputFunctionCallItem.Name+" call")
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
	out := "the model answered without a usable tool call: " + status
	if len(items) == 0 {
		return out + ", no output"
	}
	return out + "; " + strings.Join(items, ", ")
}

// streamTries is how often a request is sent when its stream breaks off.
const streamTries = 3

// wholeResponses reads each streamed answer to its end before go-agent sees
// it, which lets it fix what go-agent handles badly:
//   - A tool call whose arguments aren't valid JSON is silently dropped by
//     go-agent; an answer without calls then ends the session. Its
//     arguments become {"_broken_arguments": "<what's wrong>"}, which the
//     tool answers with an error, so the model can send it again.
//   - An incomplete answer (e.g. at the output limit) counts as a missing
//     answer; it is passed on, so its reason shows in the log.
//   - A failed answer or error event becomes an error with its message.
//   - A stream that breaks off is tried again.
type wholeResponses struct{ next agent.ResponseSender }

func (w wholeResponses) SendResponse(ctx context.Context, req components.ResponsesRequest, md *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error) {
	for try := 1; ; try++ {
		res, err := w.next.SendResponse(ctx, req, md, opts...)
		if err != nil || res == nil {
			return res, err
		}
		if res.EventStream == nil {
			if res.OpenResponsesResult != nil {
				markBrokenArguments(res.OpenResponsesResult)
			}
			return res, nil
		}
		resp, err := readStream(res)
		var broke errStreamBroke
		if errors.As(err, &broke) && try < streamTries && ctx.Err() == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		markBrokenArguments(&resp)
		return &operations.CreateResponsesResponse{OpenResponsesResult: &resp, Type: operations.CreateResponsesResponseTypeOpenResponsesResult}, nil
	}
}

type errStreamBroke struct{ err error }

func (e errStreamBroke) Error() string {
	if e.err != nil {
		return "the answer broke off: " + e.err.Error()
	}
	return "the answer broke off before it was complete"
}

func readStream(res *operations.CreateResponsesResponse) (components.OpenResponsesResult, error) {
	st := res.EventStream
	defer st.Close()
	for st.Next() {
		v := st.Value()
		if v == nil {
			continue
		}
		switch ev := v.GetData(); {
		case ev.StreamEventsResponseCompleted != nil:
			return ev.StreamEventsResponseCompleted.Response, nil
		case ev.StreamEventsResponseIncomplete != nil:
			return ev.StreamEventsResponseIncomplete.Response, nil
		case ev.StreamEventsResponseFailed != nil:
			msg := "unknown error"
			if e := ev.StreamEventsResponseFailed.Response.Error; e != nil {
				msg = e.Message
			}
			return components.OpenResponsesResult{}, fmt.Errorf("the model failed: %s", msg)
		case ev.ErrorEvent != nil:
			return components.OpenResponsesResult{}, fmt.Errorf("the model failed: %s", ev.ErrorEvent.Message)
		}
	}
	return components.OpenResponsesResult{}, errStreamBroke{st.Err()}
}

func markBrokenArguments(resp *components.OpenResponsesResult) {
	for _, it := range resp.Output {
		fc := it.OutputFunctionCallItem
		if fc == nil || strings.TrimSpace(fc.Arguments) == "" {
			continue
		}
		err := json.Unmarshal([]byte(fc.Arguments), new(any))
		if err == nil {
			continue
		}
		msg := err.Error()
		var se *json.SyntaxError
		if errors.As(err, &se) {
			r := []rune(fc.Arguments[:min(int(se.Offset), len(fc.Arguments))])
			msg += fmt.Sprintf(" at byte %d of %d, after %q", se.Offset, len(fc.Arguments), string(r[max(0, len(r)-60):]))
		}
		b, _ := json.Marshal(map[string]string{"_broken_arguments": msg})
		fc.Arguments = string(b)
	}
}
