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
  Several apps means several instances.
- **Plain server-rendered pages.** Every feature works without JavaScript.
- **Few dependencies.** The Go standard library plus a pure-Go SQLite driver.
- **Known contributors.** Admins create every account, so there is no sign-up,
  spam filtering or approval queue. History and restore cover mistakes.

## Stack

| Part | Choice | Why |
|---|---|---|
| Language | Go 1.26 | Single static binary, stdlib covers almost everything |
| Routing | `net/http` `ServeMux` | Method and path patterns (`GET /t/{lang}/{key}`) are built in |
| CSRF | `http.CrossOriginProtection` | Rejects cross-origin POSTs via `Sec-Fetch-Site`/`Origin`, no tokens in forms |
| Templates | `html/template` | Auto-escaping, no build step |
| Assets | `embed.FS` | Templates, CSS and JS ship inside the binary |
| Database | SQLite via `modernc.org/sqlite` | Pure Go, no cgo, cross-compiles; one file to back up |
| Queries | plain `database/sql` | A few dozen queries; an ORM or codegen costs more than it saves |
| Migrations | embedded `schema/NNN_*.sql` + `PRAGMA user_version` | No migration tool |
| Passwords | `crypto/pbkdf2`, SHA-256, 600k iterations | No extra dependency |

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
- A source import updates texts and hashes and marks keys that disappeared as
  removed. They stay in the history but are no longer exported.
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

## Data model

Schema: [`internal/store/schema/001_init.sql`](../internal/store/schema/001_init.sql).

- **Outdated** means `translations.source_hash != source_strings.hash`. There is
  no status column to keep in sync.
- **Saving** inserts a `history` row and updates `translations` in one
  transaction. History is append-only.
- **Restoring** an old version saves its text again as a new version.
- **Edit conflicts:** the editor form carries the history id it showed. If
  someone saved in between, both texts are shown instead of silently
  overwriting.
- **Origins:** `import`, `user`, and `machine` (reserved for machine translation).

## Accounts

- The first admin is created on the command line, which prints an invite link.
- Admins add users in the UI and pass on a one-time invite link (7 days). There
  is no email sending. Password resets work the same way.
- Sessions: 32 random bytes in an `HttpOnly`, `SameSite=Lax` cookie (`Secure`
  behind HTTPS), stored as SHA-256, 30 days sliding. Disabling a user ends
  their sessions.
- Failed logins are limited to 10 per IP and email in 15 minutes.
- API tokens (`st_…`) are stored as SHA-256 and give full API access.

## Roadmap

| Feature | Note |
|---|---|
| Machine translation of missing and outdated strings | Optional; `origin = 'machine'` exists |
| External auth (OIDC, app handoff) | Map identities to `users` rows |
| Skip unfinished source strings | E.g. `@key` metadata or a key pattern, so nobody translates placeholders |
| Per-language permissions | Only if needed |
| Review mode | Edits wait for approval |
| Glossary | Shown in the editor |
| More formats | JSON, iOS `.xcstrings`, Android XML |
| Inline editing in the list | Would justify more JavaScript |
