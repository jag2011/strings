package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

// Limits for the tools, so one result never floods the context.
const (
	defaultBatch = 40
	maxBatch     = 100
	maxMatches   = 20
)

// job is a run while it works: the run plus what this attempt has seen.
type job struct {
	run        *store.AIRun
	st         *store.Store
	langName   string
	sourceName string // the source language
	prompt     string // the system prompt, before filling in variables
	// offered holds the keys next_strings returned, so a string the agent
	// can't get right isn't handed out again and again. A restarted run
	// starts with an empty set and tries those strings once more.
	offered map[string]bool
	// failed holds keys whose last save had errors.
	failed map[string]bool
	saved  int // strings saved in the current session
	log    func(ctx context.Context, format string, args ...any)
}

func (j *job) saveJob() store.AISaveJob {
	return store.AISaveJob{Lang: j.run.Lang, AIUserID: j.run.UserID, Input: j.run.Input, Suggest: j.run.Suggest}
}

// left returns the pending strings next_strings hasn't handed out yet.
func (j *job) left(ctx context.Context) ([]store.AIString, error) {
	all, err := j.st.AIPending(ctx, j.run.Lang, j.run.UserID, j.run.Input)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, a := range all {
		if !j.offered[a.Key] {
			out = append(out, a)
		}
	}
	return out, nil
}

func (j *job) mode() string {
	switch {
	case j.run.Suggest && j.run.Input == store.AIInputAll:
		return "Translate every string that isn't approved. Everything you save becomes a suggestion for a person to review."
	case j.run.Suggest:
		return "Translate the strings that have no translation yet. Everything you save becomes a suggestion for a person to review."
	case j.run.Input == store.AIInputAll:
		return "Translate every string that isn't approved. Strings without a translation get yours; for the others, yours becomes a suggestion next to the current translation."
	}
	return "Translate the strings that have no translation yet. Yours becomes the translation, waiting for approval."
}

func (j *job) tools() []Tool {
	return []Tool{
		{
			Name:        "get_job",
			Description: "Returns the job: target language, mode and how many strings are left.",
			Schema:      object(map[string]any{}),
			Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
				left, err := j.left(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"language":     fmt.Sprintf("%s (%s)", j.langName, j.run.Lang),
					"mode":         j.mode(),
					"strings_left": len(left),
				}, nil
			},
		},
		{
			Name: "next_strings",
			Description: "Returns the next strings to translate as an ARB object: each key with its source text and its \"@key\" metadata " +
				"(description, placeholders). \"current\" holds existing translations, which yours would be a suggestion for. " +
				"Each string is handed out once; \"left\" counts the strings after these.",
			Schema: object(map[string]any{
				"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("How many strings, 1 to %d. %d is a good batch.", maxBatch, defaultBatch)},
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Limit int }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if in.Limit <= 0 {
					in.Limit = defaultBatch
				}
				left, err := j.left(ctx)
				if err != nil {
					return nil, err
				}
				batch := left[:min(in.Limit, maxBatch, len(left))]
				f := &arb.File{}
				current := map[string]string{}
				for _, a := range batch {
					j.offered[a.Key] = true
					f.Messages = append(f.Messages, arb.Message{Key: a.Key, Text: a.Source, Meta: a.Meta})
					if a.HasCurrent {
						current[a.Key] = a.Current
					}
				}
				j.log(ctx, "next_strings: %d %s, %d left", len(batch), plural(len(batch), "string", "strings"), len(left)-len(batch))
				return map[string]any{
					"strings": json.RawMessage(arb.Write(f)),
					"current": current,
					"left":    len(left) - len(batch),
				}, nil
			},
		},
		{
			Name: "lookup",
			Description: "Searches the existing translations of this language by key, source text or translated text, " +
				"to keep terms consistent. Approved translations come first; prefer their wording.",
			Schema: object(map[string]any{
				"query": map[string]any{"type": "string", "description": "A word or phrase, in the source language or the target language."},
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Query string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if strings.TrimSpace(in.Query) == "" {
					return map[string]any{"error": "The query is empty."}, nil
				}
				ms, err := j.st.AILookup(ctx, j.run.Lang, in.Query, maxMatches)
				if err != nil {
					return nil, err
				}
				type match struct {
					Key         string `json:"key"`
					Source      string `json:"source"`
					Translation string `json:"translation"`
					Approved    bool   `json:"approved,omitempty"`
					Outdated    bool   `json:"outdated,omitempty"`
				}
				out := []match{}
				for _, m := range ms {
					out = append(out, match{m.Key, m.Source, m.Text, m.Approved, m.Outdated})
				}
				j.log(ctx, "lookup %q: %d %s", in.Query, len(out), plural(len(out), "match", "matches"))
				return map[string]any{"matches": out}, nil
			},
		},
		{
			Name: "save",
			Description: "Saves translations. Each is checked like in the editor: placeholders, plural and select syntax. " +
				"Returns the result per key; fix the ones with errors and save them again. Warnings are saved anyway; " +
				"look at them and save again only if the translation is really wrong.",
			Schema: object(map[string]any{
				"translations": map[string]any{
					"type": "array",
					"items": object(map[string]any{
						"key":  map[string]any{"type": "string"},
						"text": map[string]any{"type": "string", "description": "The translation, in ICU message format like the source."},
					}),
				},
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Translations []store.AIText }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				res, err := j.st.AISave(ctx, j.saveJob(), in.Translations)
				if err != nil {
					return nil, err
				}
				return j.record(ctx, res)
			},
		},
	}
}

// record counts and logs what save did and builds its result.
func (j *job) record(ctx context.Context, res []store.AISaved) (any, error) {
	type result struct {
		Key      string   `json:"key"`
		Result   string   `json:"result"`
		Errors   []string `json:"errors,omitempty"`
		Warnings []string `json:"warnings,omitempty"`
	}
	out := []result{}
	var translated, suggested, same, warned int
	var errKeys []string
	for _, r := range res {
		out = append(out, result{r.Key, r.Result, r.Errors, r.Warnings})
		if r.Result == store.AIError {
			errKeys = append(errKeys, r.Key)
			// Unknown or approved keys never become pending, so they
			// aren't counted as failed strings of the job.
			if j.offered[r.Key] {
				j.failed[r.Key] = true
			}
			continue
		}
		delete(j.failed, r.Key)
		j.saved++
		switch r.Result {
		case store.AITranslated:
			translated++
		case store.AISuggested:
			suggested++
		case store.AISame:
			same++
		}
		if len(r.Warnings) > 0 {
			warned++
		}
	}
	if err := j.st.AddAIRunCounts(ctx, j.run.ID, translated, suggested, len(j.failed)); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("saved %d: %d ok", len(res), len(res)-len(errKeys))
	if warned > 0 {
		msg += fmt.Sprintf(" (%d with warnings)", warned)
	}
	if same > 0 {
		msg += fmt.Sprintf(", %d same as the current translation", same)
	}
	if len(errKeys) > 0 {
		msg += fmt.Sprintf(", %d %s: %s", len(errKeys), plural(len(errKeys), "error", "errors"), shortList(errKeys, 5))
	}
	j.log(ctx, "%s", msg)
	return map[string]any{"results": out}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func shortList(keys []string, n int) string {
	if len(keys) <= n {
		return strings.Join(keys, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(keys[:n], ", "), len(keys)-n)
}
