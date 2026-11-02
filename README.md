# strings

Demo: https://homequest.app/strings

Minimalistic, self-hosted translation management for ARB files.

Upload your source ARB, let translators work in a clean web interface, download
translated ARBs. One binary, one SQLite file, no JavaScript framework.

- **ARB in, ARB out.** Key order, metadata and formatting are kept. Exporting
  unchanged files gives byte-identical output, so your diffs stay quiet.
- **Built for translators.** Focus mode with *Save & next*, keyboard only if
  you like, descriptions and placeholders from `@key` metadata, spell checking
  in the target language, full history with one-click restore.
- **Drafts stay hidden.** Mark unfinished source strings with
  `"@key": {"x-draft": true}` and translators won't see them until you drop
  the tag.
- **Several files per project.** The app strings and the store listing,
  say, each with their own keys, sharing languages, translators and AI
  users. Files keep their paths in your repository.
- **Outdated detection.** When the source text changes, its translations are
  flagged until someone checks them. Uploading a new source in the web UI
  shows a review first, where typo fixes can keep their translations.
- **ICU-aware validation.** Broken braces, unknown placeholders and lost
  plurals are caught before saving. Language-specific plural categories are
  fine.
- **Proofreading.** Proofreaders tick *Approved* on translations they've
  checked. Approved strings get a checkmark and are locked until a
  proofreader or admin removes the approval.
- **Simple accounts.** Admins invite translators and proofreaders with a
  link. No email server. Optionally, users of your app sign in with their
  app account through a link the app's backend emails them.
- **AI translation.** AI users with models from any provider (through
  OpenRouter) translate a language in the background. Their work waits for
  approval, as translations or as suggestions to pick from.
- **HTTP API** for syncing from CI with `curl`.
- **Small.** Go standard library plus a pure-Go SQLite driver, and
  OpenRouter's Go agent SDK for AI translation. Works without JavaScript; a
  few lines of it add keyboard shortcuts.

## Quick start

Requires Go 1.26 or newer.

```sh
go install github.com/jag2011/strings/cmd/stringsd@latest
```

or from a checkout: `go build -o bin/stringsd ./cmd/stringsd`.

```sh
# Create the first admin. This prints a link to set the password.
stringsd user -admin -url http://localhost:8080 you@example.com "Your Name"

# Import existing files from your app's repository. A file in the source
# language (default: en) becomes the source of a file; the others seed
# translations.
cd path/to/app
stringsd import -db ~/strings.db lib/l10n/*.arb fastlane/listing/*.arb

stringsd serve -db ~/strings.db
```

Open the printed link, set a password, and you're in.

## Files

A project can have several source files, like the app strings and the store
listing. Each file has its own keys: the same key in two files is two
different strings. Languages, users, AI users and settings are shared.

A file is identified by a path template relative to the repository root,
with `{locale}` for the language: `lib/l10n/intl_{locale}.arb`. The language
codes are the Flutter locale codes from the file names (`de`, `pt_BR`,
`zh_Hant`).

- Importing a source file that fits no template creates a file: the path with
  the language replaced, named after the file name prefix (`store_en.arb` →
  `store`, `en.arb` → `app`). Translations need their source file first.
- Files uploaded one by one in the browser carry no folders, so they are
  matched by their file name alone, which works as long as no two files have
  the same name. A zip keeps its folders; its paths are relative to the
  repository root, like the exports.
- *Settings → Files* changes a file's path, name and order. A file nobody
  has worked on yet, like a mistaken upload, can be deleted.
- Exports write each file's translations at their paths: the zip unpacks at
  the repository root.

## Commands

The server command is `stringsd`, so it doesn't clash with the Unix `strings`
tool. All commands take `-db path` (default `strings.db`, or the `STRINGS_DB`
environment variable).

| Command | Does |
|---|---|
| `serve [-addr :8080]` | Runs the web app and API (`STRINGS_ADDR`) |
| `user [-admin \| -proofreader] [-url …] <email> <name>` | Creates a user and prints an invite link |
| `import [-root dir] <files…>` | Imports ARB files; paths are taken relative to `-root` (default: the working directory). Files outside it are matched by their file name |
| `export [-out dir] [-file name]` | Writes the translations of every file, or of one, at their paths under `-out` |
| `token <name>` | Creates an API token |

## Syncing from your app's repository

Create a token under *Settings → API tokens* or with `stringsd token ci`, then,
at the root of your app's repository:

```sh
H="Authorization: Bearer $STRINGS_TOKEN"

# Push the source strings of each file
for f in lib/l10n/intl_en.arb fastlane/listing/store_en.arb; do
  curl -fsS -X PUT -H "$H" --data-binary @"$f" "$STRINGS_URL/api/v1/files/$f"
done

# Pull the translations of all files
curl -fsS -H "$H" -o strings.zip "$STRINGS_URL/api/v1/files.zip" && unzip -o strings.zip && rm strings.zip
```

Paths in the API are relative to the repository root, as in the file
templates. A path without a folder also matches a template by its file name.

| Endpoint | |
|---|---|
| `PUT /api/v1/files/{path}` | Body: ARB. A source path, like `lib/l10n/intl_en.arb`, imports the source of its file, or creates the file; it returns the file, `new_file`, the new revision and added, changed, removed and unchanged counts. Translations of changed strings become outdated, except for keys listed in `keep` or `remove` parameters (`?keep=a&keep=b&remove=c`). With `revision` from a preview, it fails with 409 if the file's source changed since. A translation path, like `lib/l10n/intl_de.arb`, seeds that language; it never overwrites edits or approvals made in strings, nor brings back texts changed or removed there (`stale`) |
| `POST /api/v1/preview/{path}` | Body: source ARB. Shows what the `PUT` would change, like the review in the web UI: `new_file`, the revision, added and removed keys, and the changed strings with their old and new text, number of translations and whether they were drafts. Changes nothing |
| `GET /api/v1/files/{path}` | One translated file |
| `GET /api/v1/files.zip` | All translated files at their paths; `?file=store` for those of one file |
| `GET /api/v1/files` | The files: name, path template, revision and number of strings |
| `GET /api/v1/stats` | Done, outdated, missing and approved per language, in total and per file (`files`) |
| `GET /api/v1/contributors` | Per language: contributor names and number of strings, for credits |

Deprecated, from before there were several files: `PUT /api/v1/source`,
`POST /api/v1/source/preview` and `PUT /api/v1/translations/{lang}` work on
the first file. `GET /api/v1/arb/{name}.arb` and `GET /api/v1/arb.zip` find
files by their file name and leave out the folders.

The source files are never exported. They belong to your repository.

## How translations are handled

- **Missing** translations are left out of exports, so your app falls back to
  the source language.
- **Approved** translations can only be changed after a proofreader or admin
  unchecks *Approved*. Proofreaders find the rest under the *unapproved*
  filter. When the source text changes, the approval goes with the outdated
  flag, unless the change is kept in the import review.
- **Outdated** translations (the source text changed since) are exported and
  flagged. Saving one, even unchanged, marks it up to date.
- **Validation** follows ICU MessageFormat as Flutter uses it (apostrophes are
  literal). Errors block saving: empty text, broken braces, unknown
  placeholders, a plural or select turned into a plain placeholder. Warnings
  need a second click: unused placeholders, a different number of line
  breaks, text much longer than the source.
- **Imports** never overwrite edits made in strings, and never bring back a
  text that was changed or removed in strings since, as from a file older than
  the last export. Strings with errors are skipped and reported. Empty ones
  and copies of the source text (what many tools put in for untranslated
  strings) are treated as missing. A copy that is right, like "Tank" in
  German, is confirmed by saving it in strings once: the export writes it, and
  later imports keep it.

More in [docs/design.md](docs/design.md).

## Sign-in links from your app

Let the users of your app sign in with their app account. In the app, they
tap a button, your backend emails them a link, and the link signs them in to
strings on any device. New users become translators. Their name and email
follow their app account; an existing strings user with the same email is
linked.

1. Your backend makes a one-time code (short-lived, random) for the user and
   emails a link to `https://strings.example.com/auth/app?code=…`.
2. The link opens a page with a *Continue* button (mail scanners open links,
   so the code isn't used on `GET`). The button makes strings exchange the
   code:

   ```http
   POST $STRINGS_AUTH_URL
   Authorization: Bearer $STRINGS_AUTH_SECRET
   Content-Type: application/json

   {"code": "…"}
   ```

   Your backend deletes the code and answers `200` with
   `{"id": "…", "name": "…", "email": "…"}`, or `404` if the code is unknown,
   used or expired.

Enable it with:

```sh
STRINGS_AUTH_URL=https://api.example.com/w/redeem
STRINGS_AUTH_SECRET=…          # shared with your backend
STRINGS_AUTH_NAME="My App"     # shown on the login page
```

## AI translation

AI users translate through [OpenRouter](https://openrouter.ai), which reaches
models of many providers with one key. Enable it with:

```sh
OPENROUTER_API_KEY=sk-or-…
```

1. Under *Users*, add an AI user with an OpenRouter model name, like
   `anthropic/claude-opus-5.5` or `openai/gpt-5.5`. The model field offers
   OpenRouter's current models that can call tools, with their prices
   (loaded from its public model list, refreshed every 6 hours).
2. Under *AI → Jobs*, pick a file (if there are several), a language, the
   AI user, *Missing* or *All* strings and a cost limit, and press
   *Translate*. The run starts in the background; its page shows progress,
   cost and a log.

The instructions for the model are under *AI → Prompt*: a built-in prompt
with the ARB/ICU rules and how to work, which you can edit, e.g. to describe
your app or its tone. `{{file}}`, `{{language}}`, `{{source}}`, `{{code}}` and
`{{job}}` are filled in per run, so a prompt can treat the store listing
differently, and each run keeps the prompt it ran with. A run translates one
file; when it looks up existing translations to keep terms consistent, it
searches all files.

What a run may do is enforced when it saves, whatever the model asks:
missing strings get the AI user's translation (or a suggestion, with *Save as
suggestions*); existing translations only ever get suggestions, and only with
*All*; approved strings are never touched. Texts with validation errors are
not saved, and the model is told to fix them. Everything is credited to the
AI user and waits for a proofreader.

A run stops when nothing is left, at its cost limit, or when cancelled.
Runs that a server restart interrupted can be started again and continue
where they stopped. Several AI users can translate the same language; their
suggestions appear side by side in the editor.

*AI → Fill* shows per language what an AI user has left to suggest in a file. Tick
languages to create fill jobs for its missing strings, or to promote its
suggestions: they become the translations of missing and outdated strings,
unapproved, and wait for a proofreader like any other machine translation.

## Running in production

- Put it behind a reverse proxy that terminates TLS and sets
  `X-Forwarded-Proto: https`, so session cookies are marked `Secure`.
- If the proxy appends the client IP to `X-Forwarded-For` (AWS load balancers
  do), set `STRINGS_TRUST_PROXY=1` so login limits apply per client rather than
  per proxy.
- To share a host with other apps, serve strings under a path with
  `STRINGS_BASE=/strings`: the proxy forwards `/strings/*` unchanged and links,
  redirects and cookies get the prefix. API clients and sign-in links then
  use `https://example.com/strings` as the base URL.
- `STRINGS_PUBLIC=1` (or `serve -public`) makes the app viewable without a
  login: visitors can browse the languages, strings, activity and profiles
  (without emails), read-only. Editing, admin pages and exports still need a
  login.
- `GET /health` returns 200 while the database answers, for load balancer
  health checks. With a base path it is also served at the root.
- `stringsd backup <out.db>` writes a consistent single-file copy of the
  database, also while the server runs.
- [`deploy/strings.service`](deploy/strings.service) is a systemd unit that
  keeps the database in `/var/lib/strings` and reads secrets from
  `/etc/strings/strings.env`; see
  [`deploy/strings.env.example`](deploy/strings.env.example).
- All data is in the SQLite file plus its `-wal` file. Back up with
  `sqlite3 strings.db ".backup backup.db"`, or stream it with
  [Litestream](https://litestream.io).
- `SIGTERM` shuts down gracefully.

## Development

```sh
go test ./...
go vet ./...

# Also check that your own ARB files round-trip
ARB_TESTDATA=path/to/lib/l10n go test ./internal/arb
```

| Path | |
|---|---|
| `cmd/stringsd` | CLI |
| `internal/arb` | ARB parsing and writing, ICU validation |
| `internal/ai` | AI translation runs: worker, agent tools, OpenRouter |
| `internal/store` | SQLite storage; migrations in `schema/` |
| `internal/web` | Handlers, `templates/`, `static/` |

With `OPENROUTER_API_KEY` set, `go test ./internal/ai` also calls the real API
once (cheap model, a few cents at most; pick one with `OPENROUTER_TEST_MODEL`).

Issues and pull requests are welcome. Please keep the spirit: few dependencies,
plain HTML, no build step.

## License

[MIT](LICENSE)
