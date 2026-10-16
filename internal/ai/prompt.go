package ai

import (
	"fmt"
	"strings"
)

// systemPrompt stays the same for all sessions of a job, so providers can
// cache it.
func systemPrompt(j *job) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You translate the user interface strings of an app from %s into %s (%s). You work only through the tools; people review your work afterwards.

Job: %s
`, j.sourceName, j.langName, j.run.Lang, j.mode())
	if j.run.Notes != "" {
		fmt.Fprintf(&b, "\nStyle notes from the team, follow them:\n%s\n", j.run.Notes)
	}
	b.WriteString(`
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
`)
	return b.String()
}
