# Plan: several source files per project

Status: implemented (schema version 11). See the README for usage.

A project can have more than one source ARB, e.g. the app strings and the
store listing of the same app:

```
lib/l10n/intl_en.arb             → lib/l10n/intl_de.arb, …
fastlane/listing/store_en.arb    → fastlane/listing/store_de.arb, …
```

Each file is a separate set of keys: the same key may exist in two files and
means two different strings. Everything else stays shared: languages, users,
AI users, the prompt, settings.

Out of scope for now: other formats than ARB, locale mapping on export (store
locale codes, one language to several locales), languages per file. The path
template below is meant to leave room for them.

## Files

A file is identified by a **path template**: a path relative to the
repository root with one `{locale}` placeholder.

| Column | Example | |
|---|---|---|
| `id` | `2` | Referenced by all other tables, so a path can change without touching data |
| `path` | `fastlane/listing/store_{locale}.arb` | Unique. Relative, `/`-separated, no `..`, one `{locale}`, ends in `.arb` |
| `name` | `store` | Unique, short, `[a-z0-9_-]`. Used in URLs and labels |
| `position` | `2` | Order in lists, exports and "Save & next" |
| `revision` | `5` | Source revision, per file |
| `arb_locale` | `0` | Write `@@locale`: follows the source file, as today |
| `trailing_newline` | `1` | Follows the source file, as today |

These replace the settings `file_prefix`, `source_revision`, `arb_locale` and
`arb_trailing_newline`, which already describe the one source file rather than
the instance. `source_lang` stays global.

**Matching a path.** A concrete path (`fastlane/listing/store_de.arb`) is
matched against every template; `{locale}` must match a valid locale
(`arb.ValidLocale`). The result is (file, language). If the language is the
source language, the path is that file's source.

- Templates may not overlap: a new or edited template is refused if a path
  of it for any language would match another template.
- **Loose uploads** (web form, `stringsd import` without a root) only carry
  base names, see below. They are matched by the template's base name, which
  works as long as base names differ (`intl_…` vs `store_…`); if a base name
  matches more than one file, the upload asks for a zip.

**New files.** A source-language path that matches no template creates a new
file. Its template is the path with the locale replaced: directory +
`arb.SplitFilename` prefix + `{locale}.arb`; its name is the prefix without
trailing `_.-` (`store_` → `store`, empty → `app`). The web review and the API
preview say "new file" before anything is stored. A translation path that
matches no file is an error ("import the source file first").

**Admin.** *Settings* gets a *Files* section instead of the prefix field: path
and name editable, order changeable, and *Delete* only for a file without any
history (a mistaken upload). Nothing else deletes files.

## Data model

Migration `011_files.sql`, run with foreign keys off like `004_approval.sql`;
`PRAGMA foreign_key_check` afterwards catches mistakes.

1. Create `files`; insert file 1 from the current settings: path
   `<file_prefix>{locale}.arb` (no directory; the admin edits it to
   `lib/l10n/intl_{locale}.arb` after the update), revision, flags. Delete the
   four settings rows and drop them from `defaults` in `store.go`.
2. Rebuild with `file_id INTEGER NOT NULL REFERENCES files(id)`, existing rows
   get `1`:
   - `source_strings`: primary key `(file_id, key)`.
   - `translations`: primary key `(lang, file_id, key)`, foreign key
     `(file_id, key) → source_strings`.
   - `suggestions`: `UNIQUE (lang, file_id, key, user_id)`, foreign key as
     above.
3. Add `file_id NOT NULL DEFAULT 1` to `history`, `approval_log` and
   `ai_runs`; replace the indexes `history_key` and `approval_log_key` with
   `(lang, file_id, key)`.

SQLite can't change a primary key in place, so step 2 is create new → copy →
drop → rename, as for `users` in 004.

**Store API.** Every function that takes `(lang, key)` also takes the file id,
and every join between `translations`, `source_strings`, `history`,
`suggestions` and `approval_log` gets `AND x.file_id = y.file_id`. That is
most of the ~30 queries in `strings.go`, `suggestions.go` and `airuns.go`,
including the less obvious ones: `DeleteUserVersions` and the user merge
(which restore earlier versions per string), `logSourceUnapproval`,
`AILookup`, `Activity`.

New: `File` type, `Files`, `File(id)`, `FileByName`, `MatchPath(path) (File,
lang, ok)`, `CreateFile`, `UpdateFile`, `DeleteFile`, `(File).Path(lang)`.

Changed signatures, roughly:

| Today | Then |
|---|---|
| `PreviewSource(f)`, `ImportSource(f, decide)` | `(file, f, …)`: diff, removals and revision **only within that file** |
| `ImportTranslations(lang, f)` | `(file, lang, f)`: keys of other files are `Unknown` |
| `ImportFiles(files)`, `PreviewFiles(files)` | Keys are paths; one diff per source file in the upload |
| `Export(lang)` | `Export(file, lang)` |
| `Stats()` | Totals per language as today, plus per file |
| `List`, `FilterCounts`, `Next`, `Keys` | Optional file filter; order by file position, then key position |
| `Save`, `Revert`, `DeleteVersion`, `Entry`, suggestions | Take the file |
| `AIPending`, `AISave`, `AIFillStats`, `PromoteSuggestions` | Take the file |

Scoping the source import to its file is the key change: today importing any
source file marks every key not in it as removed.

## Import and export

**Web upload** (*Settings → Import*):

- Plain `.arb` files are matched by base name (Go's `multipart.Part.FileName`
  strips directories, and browsers don't reliably send them).
- A `.zip` keeps its paths and is matched against the full templates. This is
  the way to upload a folder structure, and works without JavaScript.
- The review page (`/admin/import/{id}`) shows one section per source file:
  its name, "new file" if so, and the existing diff with keep / outdated /
  remove per changed string. Decisions are keyed by file and key. Confirming
  checks each file's revision.

**Export** (logged in): `GET /export/{path...}` for one file,
`GET /export.zip` for all files of all target languages at their paths, so
it unzips at the repository root. The source file stays unexported, as today.

**CLI:**

- `stringsd import [-root dir] <files…>`: paths are taken relative to
  `-root` (default: the working directory), so
  `stringsd import lib/l10n/*.arb fastlane/listing/*.arb` at the repo root
  gets the right paths.
- `stringsd export [-out dir] [-file name]`: writes the tree under `-out`.

## API

| Endpoint | |
|---|---|
| `PUT /api/v1/files/{path...}` | Body: ARB. A source path imports the source of that file (or creates the file), with `keep`, `remove` and `revision` as today. A translation path seeds that language, as `PUT /translations/{lang}` today |
| `POST /api/v1/preview/{path...}` | What the `PUT` of a source path would change, plus `new_file` |
| `GET /api/v1/files/{path...}` | One translated file |
| `GET /api/v1/files.zip` | All translated files at their paths; `?file=store` for one file |
| `GET /api/v1/files` | The files: name, path template, revision, string count |
| `GET /api/v1/stats` | As today (totals), plus `files: {name: {…}}` per language |

`{path...}` has to be the last segment in `ServeMux` patterns, hence
`/preview/{path...}` instead of `…/preview`.

The current endpoints keep working and refer to the first file:
`PUT /source`, `POST /source/preview`, `PUT /translations/{lang}`. `GET
/arb/{file}` and `/arb.zip` match by base name across all files and stay
flat. The README documents the new ones and calls the old ones deprecated.

Pulling in CI becomes:

```sh
curl -fsS -H "$H" -o strings.zip "$STRINGS_URL/api/v1/files.zip" && unzip -o strings.zip && rm strings.zip
```

## Web UI

- **Routes:** `/t/{lang}/{file}/{key}` and the save, revert, delete and
  suggestion routes under it. The old `GET /t/{lang}/{key}` redirects to the
  first file that has the key, so bookmarks and shared links keep working.
  The old POST routes go away; their patterns would conflict in `ServeMux`.
- **List** (`/t/{lang}`): all files by default, in file order, each row with
  the file name as a muted label. File chips next to the filter set
  `?file=store`, kept in the URL like `filter` and `q`. Filter counts and
  "Save & next" follow it.
- **Editor:** the file name and its path above the source text.
- **Home:** totals per language as today; with more than one file, the same
  chips switch the table to one file.
- **Activity, history, profile:** the file label on each line, only when
  there is more than one file.
- With a single file, labels and chips are hidden, so instances with one file
  look as they do today.

About 33 places in 10 templates and `web.go`/`pages.go` build `/t/…` links;
a template helper `strURL lang file key` keeps that in one place.

## AI

- A run translates one file: `ai_runs.file_id`, a file select on *AI → Jobs*
  and *AI → Fill* (`?file=`). Active runs are per (file, language).
- `get_job` names the file; `next_strings` and `save` work within it, so keys
  stay unambiguous for the model.
- `lookup` searches all files and returns the file name with each match, so
  game terms stay consistent between app and store text.
- New prompt variable `{{file}}` (the file's name and path), so a custom
  prompt can give the store listing its own instructions.

## Order of work

Done on one branch, merged once; each step keeps `go test ./...` green.

1. **Migration and store.** `files` table, `file_id` everywhere, store
   functions take the file; handlers pass file 1. No visible change.
   Test: migrating a version-10 database with data keeps every row and
   approval.
2. **Several files.** Templates, matching, scoped source import, per-file
   revision and export, upload review per file, zip upload and export, CLI.
3. **API.** `/files/…`, `/preview/…`, stats per file, old endpoints mapped.
4. **Web UI.** Routes with the file, redirects, chips, labels, *Files* in
   settings.
5. **AI.** Runs per file, lookup across files, `{{file}}`.
6. **Docs.** README (sync, API), `design.md`: "one instance, one project"
   stays, a project now has several files; data model section.

Tests to add on the way: two files with the same key stay independent;
importing one source doesn't remove keys of the other; revision conflicts are
per file; overlapping templates are refused; loose uploads with ambiguous base
names ask for a zip; the old routes and endpoints still work.

**Deploying:** take a backup before the update (`stringsd backup`), then set
the first file's path to `lib/l10n/intl_{locale}.arb` under *Settings → Files*
and upload `fastlane/listing/store_en.arb` (or push it to
`/api/v1/files/fastlane/listing/store_en.arb`).

## Related, separate

- `x-max-length` in `@key` metadata is stored but not checked. The store
  listing needs it: an error in `arb.Check`, a counter in the editor.
- The editor only says that the source text changed, not how. With long texts
  like the store description, a diff against the previous source text would
  help; that needs the source text history, which isn't kept today.
