# Design

How strings works and why. For usage, see the [README](../README.md).

```
source ARB ──PUT──▶ strings (SQLite) ──GET──▶ translated ARBs
                       ▲
          translators  │ web forms
```

The database is the source of truth for translations. Translated ARB files are
build output and are not edited by hand. The source file stays owned by the
app's repository and is never written by strings.

## Principles

- **One instance, one SQLite file, one project.** There is no project layer.
  Several apps means several instances. A project can have several source
  files, like the app strings and the store listing of the same app.
- **Plain server-rendered pages.** Every feature works without JavaScript.
- **Few dependencies.** The Go standard library plus a pure-Go SQLite driver,
  and OpenRouter's agent SDK for AI translation.
- **Known contributors.** Admins create every account, or an app's backend
  vouches for its users (sign-in links), so there is no sign-up, spam
  filtering or approval queue. History and restore cover mistakes.

## Stack

| Part | Choice | Why |
|---|---|---|
| Language | Go 1.26 | Single static binary, stdlib covers almost everything |
| Routing | `net/http` `ServeMux` | Method and path patterns (`GET /t/{lang}/{file}/{key}`) are built in |
| CSRF | `http.CrossOriginProtection` | Rejects cross-origin POSTs via `Sec-Fetch-Site`/`Origin`, no tokens in forms |
| Templates | `html/template` | Auto-escaping, no build step |
| Assets | `embed.FS` | Templates, CSS and JS ship inside the binary |
| Database | SQLite via `modernc.org/sqlite` | Pure Go, no cgo, cross-compiles; one file to back up |
| Queries | plain `database/sql` | A few dozen queries; an ORM or codegen costs more than it saves |
| Migrations | embedded `schema/NNN_*.sql` + `PRAGMA user_version` | No migration tool |
| Passwords | `crypto/pbkdf2`, SHA-256, 600k iterations | No extra dependency |
| AI agent | [`OpenRouterTeam/go-agent`](https://github.com/OpenRouterTeam/go-agent) | Tool loop and usage over OpenRouter's Responses API; one key for many model providers |

SQLite runs with `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON` and
`synchronous=NORMAL`. Writes go through a single connection, reads through a
pool, which avoids `SQLITE_BUSY` under concurrent saves.

## User experience without a frontend framework

1. **Fast pages.** Rendering takes about a millisecond and pages are a few KB,
   so a full page load feels like an SPA update.
2. **Focus mode editor with "Save & next".** Saving redirects to the next
   string in the current filter (by default: missing or outdated) with the
   textarea focused. Translators can work through a list from the keyboard.
3. **State lives in the URL.** Filter, search and page are query parameters, so
   back, reload, bookmarks and shared links work.
4. **No white flash between pages:** `@view-transition { navigation: auto; }`.
5. **Flash messages** after redirects via a short-lived cookie.
6. **One stylesheet**, system fonts, dark mode via `prefers-color-scheme`, the
   accent color from the settings as a CSS variable.
7. **The textarea carries `lang` and `dir`**, so the browser spell-checks in the
   target language and right-to-left scripts display correctly.

`static/app.js` adds Ctrl/Cmd+Enter to save, a "Copy source" button and a
warning before leaving with unsaved edits. Nothing depends on it.

## ARB handling (`internal/arb`)

ARB is JSON with meaning in key order and in `@`-prefixed entries: `@@locale`
and other globals, `"key": "text"` messages, and `"@key": {…}` metadata.

Import:

- Parsed with `json.Decoder` tokens to keep the key order, which Go maps lose.
- `@key` metadata is stored as JSON. Its `description` and placeholder examples
  are shown to translators.
- `"x-draft": true` in `@key` metadata marks a source string as unfinished, e.g.
  placeholder text. Drafts are hidden from translators: not listed, not
  counted, not editable. The tag is re-read on every source import, so
  dropping it makes the string translatable.
- A source import updates the texts and hashes of its file and marks keys
  that disappeared from it as removed. They stay in the history but are no
  longer exported. Other files aren't touched.
- Every source import that changes texts, metadata or order raises the
  file's revision by one.
- Source files uploaded in the web UI are kept in `uploads` and shown for
  review first, one section per file: "new file", or the revision, added,
  removed and unchanged counts, and every changed string with its old and
  new text. Per changed string the admin chooses what happens to its
  translations: *keep* (still up to date, e.g. a typo fix; the default),
  *mark outdated* (the default for former drafts) or *remove* (missing again,
  history keeps them). Confirming checks that every file is still at the
  revision reviewed. The API and the `import` command don't ask and mark
  translations of changed strings outdated.
- Translation imports never overwrite strings edited in strings.
- Empty translations of non-empty source text are treated as missing, so the
  app falls back to the source language instead of showing nothing.

Export:

- `@@locale` (only if the source file has one), then keys in source order.
  Translated files get no metadata; Flutter reads it only from the template.
- Missing translations are left out. Outdated ones are included, since stale
  text beats a fallback, and are flagged in the UI.
- Written by hand with 2-space indent and without HTML escaping (Go's encoder
  turns `<` into `<`). Whether the file ends with a newline follows the
  source file. Files written by common tools round-trip byte for byte.

### Validation

ICU MessageFormat as Flutter uses it: apostrophes are literal text.

| Check | Result |
|---|---|
| Empty (when the source is not) | error |
| Braces don't parse | error |
| Placeholder not in the source | error, it would break code generation |
| A `plural`/`select` turned into a plain placeholder, or missing `other` | error |
| Different plural categories than the source | fine: Polish needs `few`/`many`, Japanese only `other` |
| Source placeholder not used | warning: sometimes intended, e.g. to spell out and inflect a name |
| Different number of line breaks | warning |
| Longer than 3× the source + 20 characters | warning |

Errors block saving. Warnings need a second click. Imports skip strings with
errors and report those with warnings.

### Files

A file is a path template relative to the repository root with one
`{locale}`: `lib/l10n/intl_{locale}.arb`, `fastlane/listing/store_{locale}.arb`.
Its id is used everywhere else, so the path and name can change without
touching data. Whether exports get `@@locale` and a trailing newline, and the
source revision, are kept per file.

- **Matching a path.** A path is matched against every template, where
  `{locale}` must be a valid locale; the result is the file and language.
  Templates may not overlap: a template is refused if one of its paths, for
  the instance's languages and a few sample locales, fits another one.
- **File names alone.** Browsers don't send folders with uploaded files, so
  a path without a folder that fits no template also matches a template's
  file name, as long as only one does. Otherwise the upload needs a zip,
  which keeps its paths.
- **New files.** A source-language path that fits no template creates a
  file: the path with the locale replaced, named after the file name prefix
  (`store_` → `store`, none → `app`). If the name is taken, the import is
  refused instead, since that's usually a file whose path wasn't updated
  under *Settings → Files*. A translation path that fits no file is an error.

## Data model

Schema: [`internal/store/schema/001_init.sql`](../internal/store/schema/001_init.sql),
files since [`011_files.sql`](../internal/store/schema/011_files.sql).

- **Strings are per file.** `source_strings` is keyed by `(file_id, key)`,
  `translations` by `(lang, file_id, key)`, and `history`, `suggestions`,
  `approval_log` and `ai_runs` carry the file. Every join between them
  matches the file as well as the key.

- **Approved** means `translations.approved_by` is set. Approved translations
  can't be saved, restored or overwritten by imports until a proofreader or
  admin removes the approval, which they can do while saving a new text. A
  proofreader changing approved text approves it again. A source import that
  outdates a translation removes its approval; *keep* leaves it. The editor
  form sends whether the string was approved when opened, so a proofreader
  doesn't silently undo an approval made in the meantime.
- **Outdated** means `translations.source_hash != source_strings.hash`. There is
  no status column to keep in sync.
- **Saving** inserts a `history` row and updates `translations` in one
  transaction. History is append-only.
- **Restoring** an old version saves its text again as a new version.
- **Edit conflicts:** the editor form carries the history id it showed. If
  someone saved in between, both texts are shown instead of silently
  overwriting.
- **Origins:** `import`, `user`, and `machine` (written by an AI user).
- **Suggestions** are alternative translations, one per user and string, e.g.
  from several AI users. They are shown in the editor while they were made for
  the current source text and differ from the current translation. *Use*
  makes one current, credited to whoever suggested it; proofreaders can also
  *Use & approve* or *Dismiss*. The list tags strings that have suggestions
  and aren't approved yet. *Promote* on the Fill page does *Use* in bulk for
  one AI user and language, for missing and outdated strings only.

## Accounts

- The first admin is created on the command line, which prints an invite link.
- Admins add users in the UI and pass on a one-time invite link (7 days). There
  is no email sending. Password resets work the same way.
- Sessions: 32 random bytes in an `HttpOnly`, `SameSite=Lax` cookie (`Secure`
  behind HTTPS), stored as SHA-256, 30 days sliding. Disabling a user ends
  their sessions.
- Roles: *translators* translate, *proofreaders* also approve translations
  and remove approvals, *admins* can do everything.
- Failed logins are limited to 10 per IP and email in 15 minutes.
- **Sign-in links from an app's backend** (optional, `STRINGS_AUTH_*`): the
  backend emails a link with a one-time code to `/auth/app`. `GET` shows a
  button, because mail scanners open links; `POST` exchanges the code at the
  backend with a shared secret for `{id, name, email}`. The user is found by
  `external_id`, else linked by email (the link went to that address), else
  created as a translator. Name and email follow the app account on every
  sign-in, so admins can't rename external users. Unknown codes count
  towards a limit of 10 per IP in 15 minutes.
- API tokens (`st_…`) are stored as SHA-256 and give full API access.
- **AI users** (`users.ai_model`) translate with a language model. They can't
  sign in (no password, placeholder email), are unlisted, and are added under
  *Users*.

## AI translation (`internal/ai`)

An admin queues a run on `/admin/ai`: file, language, AI user, *Missing* or
*All*, suggestions only or not, cost limit. A run translates one file. One worker goroutine in
`stringsd serve` runs one run at a time, oldest first.

```
Runner ──session──▶ go-agent loop ──▶ OpenRouter ──▶ the AI user's model
   │                    │ tool calls
   │                    ▼
   └─ ai_runs ◀── get_job · next_strings · lookup · save ──▶ store
```

- **Tools** are plain Go functions. `get_job` returns the file, language,
  mode and strings left. `next_strings(limit)` returns the next
  pending strings as ARB (source, `@key` metadata) plus current translations;
  each string is handed out once per run, so one the model can't get right
  doesn't loop. `lookup(query)` searches existing translations in all files, approved
  ones first, to keep terms consistent between the app and its store text. `save([{key, text}])` runs `arb.Check`
  and saves by the rules below, returning errors and warnings per key.
- **The rules live in `store.AISave`, not in the prompt:** no translation →
  the AI user's translation (origin `machine`), or a suggestion if the run
  says so; a translation → only a suggestion, and only with *All*; approved →
  never touched. A suggestion equal to the current translation is stored but
  not shown, so the string counts as done for that AI user.
- **Pending is derived from the database**: strings with no translation, and
  with *All* unapproved ones, minus those the AI user already translated or
  suggested something for, for the current source text. A run started again
  after an interruption continues where it stopped, and several runs of
  different AI users can work on the same language.
- **Sessions.** The model sees the whole conversation on every call, so a
  run is split into sessions of 16 model calls; the next one starts fresh
  with "continue". The system prompt (ARB/ICU rules, language, working
  method) stays the same, and top-level `cache_control` turns on
  Anthropic's caching, which other providers do on their own.
- **The prompt** is editable on `/admin/ai/prompt` and stored in the
  `ai_prompt` setting; empty means the built-in `ai.DefaultPrompt`, so
  saving it unchanged or resetting picks up later improvements. Variables
  in double braces (`{{file}}`, `{{language}}`, `{{source}}`, `{{code}}`, `{{job}}`)
  are filled in per run, unknown ones are refused on saving. A run reads
  the prompt when it starts and keeps the filled-in text in
  `ai_runs.prompt`, so results can be compared after editing it.
- **Stopping.** Before every model call the runner checks for cancel, the
  run's cost limit (cost as OpenRouter reports it), a step limit and the
  session size. So a cancel takes effect after the current step, and a run
  overshoots its limit by one call at most. A model that stops by itself
  without saving anything has given up, and the run ends.
- **States:** queued → running → done, failed, cancelled or interrupted.
  On start, runs still marked running are interrupted (the server stopped
  during them); interrupted, failed and cancelled runs can be started again.
  Counters, tokens and cost keep adding up across restarts. `ai_run_events`
  holds one line per tool call for the log on the run's page, which reloads
  every few seconds while the run is active.
- **Tests** script the agent (`ai.Agent` is an interface) and check the save
  rules, the tools, sessions, limits, cancel and restart. The OpenRouter
  adapter is tested against a fake Responses endpoint, and against the real
  one when `OPENROUTER_API_KEY` is set.

## Roadmap

| Feature | Note |
|---|---|
| External auth (OIDC) | Sign-in links from an app exist; OIDC would add sign-in without the app |
| Per-language permissions | Only if needed |
| Review mode | Edits wait for approval |
| Glossary | Shown in the editor |
| More formats | JSON, iOS `.xcstrings`, Android XML |
| Inline editing in the list | Would justify more JavaScript |
