# strings

Minimalistic, self-hosted translation management for ARB files.

Upload your source ARB, let translators work in a clean web interface, download
translated ARBs. One binary, one SQLite file, no JavaScript framework.

- **ARB in, ARB out.** Key order, metadata and formatting are kept. Exporting
  unchanged files gives byte-identical output, so your diffs stay quiet.
- **Built for translators.** Focus mode with *Save & next*, keyboard only if
  you like, descriptions and placeholders from `@key` metadata, spell checking
  in the target language, full history with one-click restore.
- **Outdated detection.** When the source text changes, its translations are
  flagged until someone checks them.
- **ICU-aware validation.** Broken braces, unknown placeholders and lost
  plurals are caught before saving. Language-specific plural categories are
  fine.
- **Simple accounts.** Admins invite translators with a link. No email server.
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
| `user [-admin] [-url …] <email> <name>` | Creates a user and prints an invite link |
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
| `PUT /api/v1/source` | Body: source ARB. Returns added, changed, removed and unchanged counts |
| `PUT /api/v1/translations/{lang}` | Body: ARB. Seeds a language; never overwrites edits made in strings |
| `GET /api/v1/arb/{prefix}{lang}.arb` | One language |
| `GET /api/v1/arb.zip` | All target languages |
| `GET /api/v1/stats` | Done, outdated and missing per language |
| `GET /api/v1/contributors` | Per language: contributor names and number of strings, for credits |

The source file is never exported. It belongs to your repository.

## How translations are handled

- **Missing** translations are left out of exports, so your app falls back to
  the source language.
- **Outdated** translations (the source text changed since) are exported and
  flagged. Saving one, even unchanged, marks it up to date.
- **Validation** follows ICU MessageFormat as Flutter uses it (apostrophes are
  literal). Errors block saving: empty text, broken braces, unknown
  placeholders, a plural or select turned into a plain placeholder. Warnings
  need a second click: unused placeholders, a different number of line
  breaks, text much longer than the source.
- **Imports** never overwrite edits made in strings. Strings with errors are
  skipped and reported, empty ones are treated as missing.

More in [docs/design.md](docs/design.md).

## Running in production

- Put it behind a reverse proxy that terminates TLS and sets
  `X-Forwarded-Proto: https`, so session cookies are marked `Secure`.
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
