# strings

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
- **HTTP API** for syncing from CI with `curl`.
- **Small.** Go standard library plus a pure-Go SQLite driver. Works without
  JavaScript; a few lines of it add keyboard shortcuts.

## Quick start

Requires Go 1.26 or newer.

```sh
go install github.com/jag2011/strings/cmd/stringsd@latest
```

or from a checkout: `go build -o bin/stringsd ./cmd/stringsd`.

```sh
# Create the first admin. This prints a link to set the password.
stringsd user -admin -url http://localhost:8080 you@example.com "Your Name"

# Import existing files. The file in the source language (default: en)
# becomes the source; the others seed translations.
stringsd import path/to/lib/l10n/*.arb

stringsd serve
```

Open the printed link, set a password, and you're in.

Files are matched by name: `intl_en.arb`, `app_pt_BR.arb`, `zh_Hant.arb`. The
prefix is taken from the source file, the language codes are the Flutter
locale codes from the file names.

## Commands

The server command is `stringsd`, so it doesn't clash with the Unix `strings`
tool. All commands take `-db path` (default `strings.db`, or the `STRINGS_DB`
environment variable).

| Command | Does |
|---|---|
| `serve [-addr :8080]` | Runs the web app and API (`STRINGS_ADDR`) |
| `user [-admin \| -proofreader] [-url …] <email> <name>` | Creates a user and prints an invite link |
| `import <files…>` | Imports ARB files |
| `export [-out dir]` | Writes one ARB per target language |
| `token <name>` | Creates an API token |

## Syncing from your app's repository

Create a token under *Settings → API tokens* or with `stringsd token ci`, then:

```sh
H="Authorization: Bearer $STRINGS_TOKEN"

# Push the source strings
curl -fsS -X PUT -H "$H" --data-binary @lib/l10n/intl_en.arb "$STRINGS_URL/api/v1/source"

# Pull the translations
for lang in de fr pt_BR; do
  curl -fsS -H "$H" -o "lib/l10n/intl_$lang.arb" "$STRINGS_URL/api/v1/arb/intl_$lang.arb"
done
```

| Endpoint | |
|---|---|
| `PUT /api/v1/source` | Body: source ARB. Returns the new revision and added, changed, removed and unchanged counts. Translations of changed strings become outdated |
| `PUT /api/v1/translations/{lang}` | Body: ARB. Seeds a language; never overwrites edits or approvals made in strings |
| `GET /api/v1/arb/{prefix}{lang}.arb` | One language |
| `GET /api/v1/arb.zip` | All target languages |
| `GET /api/v1/stats` | Done, outdated, missing and approved per language |
| `GET /api/v1/contributors` | Per language: contributor names and number of strings, for credits |

The source file is never exported. It belongs to your repository.

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
- **Imports** never overwrite edits made in strings. Strings with errors are
  skipped and reported. Empty ones and copies of the source text (what many
  tools put in for untranslated strings) are treated as missing. A copy that
  is right, like "Tank" in German, is confirmed by saving it in strings once:
  the export writes it, and later imports keep it.

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
- `GET /health` returns 200 while the database answers, for load balancer
  health checks. With a base path it is also served at the root.
- `stringsd backup <out.db>` writes a consistent single-file copy of the
  database, also while the server runs.
- [`deploy/strings.service`](deploy/strings.service) is a systemd unit that
  keeps the database in `/var/lib/strings` and reads secrets from
  `/etc/strings/strings.env`.
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
| `internal/store` | SQLite storage; migrations in `schema/` |
| `internal/web` | Handlers, `templates/`, `static/` |

Issues and pull requests are welcome. Please keep the spirit: few dependencies,
plain HTML, no build step.

## License

[MIT](LICENSE)
