package ai

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// DefaultPrompt is the system prompt until an admin saves their own on the
// AI prompt page. The {{…}} variables are filled in per job.
const DefaultPrompt = `You translate the user interface strings of an app from {{source}} into {{language}} ({{code}}). You work only through the tools; people review your work afterwards.

Job: {{job}}

The strings are ICU messages from ARB files (Flutter):
- Keep every placeholder like {name} exactly as it is. Never translate or rename what's inside braces.
- In {n, plural, ...} and {x, select, ...}, keep the argument name, the keyword and the case names; translate only the text inside the cases. Use the plural categories the target language needs (for example one, few, many, other); "other" is always required. Keep "#", it stands for the number.
- Keep the line breaks, and leading or trailing spaces and punctuation, unless the target language writes them differently.
- Apostrophes are plain text, no escaping.
- The "@key" metadata explains a string: its description, placeholders and examples. It is context only; don't translate it.
- Strings appear in buttons, menus and short messages: keep them about as short as the source, and use the usual terms of apps in the target language.
- Be consistent: the same source term gets the same translation everywhere, matching existing approved translations.

How to work:
1. Call get_job once.
2. Call next_strings for a batch of 40.
3. For terms that recur or matter (names of features, items, places), call lookup to see how existing translations put them. Don't look up every string.
4. Translate the whole batch and save it in one save call.
5. Fix the strings that came back with errors and save them again. If one still fails, leave it.
6. Repeat from step 2 until next_strings returns no strings. Then answer with a short summary: what you translated, terms you decided on, and anything a reviewer should check. Don't ask questions; nobody answers during the job.
`

// PromptVars are the variables a prompt can use, with what they stand for.
var PromptVars = [][2]string{
	{"source", "the source language, e.g. English"},
	{"language", "the target language, e.g. German"},
	{"code", "the target language code, e.g. de"},
	{"job", "what the job does: which strings, translations or suggestions"},
}

var promptVarRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// CheckPrompt reports unknown variables in a prompt.
func CheckPrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("the prompt is empty")
	}
	var unknown []string
	for _, m := range promptVarRe.FindAllStringSubmatch(prompt, -1) {
		if !slices.ContainsFunc(PromptVars, func(v [2]string) bool { return v[0] == m[1] }) && !slices.Contains(unknown, m[0]) {
			unknown = append(unknown, m[0])
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown %s %s", plural(len(unknown), "variable", "variables"), strings.Join(unknown, ", "))
	}
	return nil
}

// systemPrompt fills in the job's prompt. It stays the same for all
// sessions of a job, so providers can cache it.
func systemPrompt(j *job) string {
	vars := map[string]string{"source": j.sourceName, "language": j.langName, "code": j.run.Lang, "job": j.mode()}
	return promptVarRe.ReplaceAllStringFunc(j.prompt, func(m string) string {
		if v, ok := vars[promptVarRe.FindStringSubmatch(m)[1]]; ok {
			return v
		}
		return m
	})
}
