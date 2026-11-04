# data-driven-framework-caddy2

A [Caddy v2](https://caddyserver.com) HTTP handler that serves a complete web
application generated at runtime from a
[data-driven-framework-go](https://github.com/timeofmind/data-driven-framework-go)
YAML schema — no per-app code:

- **Pages** for every class: searchable, filterable, paginated lists; month
  calendars for classes with a `calendar` date; detail pages showing linked records
  and every related list (`has_many`), with leaderboards where children are ranked
  by a duration; create/edit forms with grouped fields, relation pickers and
  validation messages; delete confirmations that spell out cascades.
- **Accounts** for the schema's `auth` class: sign-up, sign-in, sessions (stored
  hashed in the database), profile editing, account deletion.
- **Access rules** straight from the schema's `access` blocks (owner, public/private
  rows, parent-governed children) applied to every page, list, picker and API call.
- **JSON API** at `/api/v1/<resource>` with a generated OpenAPI 3 document.
- **Activity uploads** (optional): FIT (Garmin, COROS, Suunto, Wahoo, Polar), GPX,
  TCX and timing-system CSV files become log entries with laps, matched to the
  day's planned workout, and posted to virtual events the athlete entered.

`sites/athleticstogether/` is the first site built on it: a public training
planner, training log and virtual-events platform.

## Build

```sh
go build -o caddy ./cmd/caddy         # standard Caddy + this module
# or, with xcaddy:
xcaddy build --with github.com/timeofmind/data-driven-framework-caddy2
```

## Caddyfile

```caddyfile
example.com {
	datadriven [<base_path>] {
		schema         <file.yaml> [<more.yaml>...]
		database       mysql|sqlite <dsn>          # or:
		database_file  mysql|sqlite <file holding the DSN>
		site_name      "My App"
		tagline        "One line under the title on the home page"
		home           <Class> [<Class>...]        # featured on the home page
		signup         on|off                      # default on
		admin          <login> [<login>...]        # e.g. emails with full access
		secure_cookies on|off                      # default on
		session_ttl    720h
		timezone       America/Vancouver           # for placing uploads on a day
		uploads        <dir> [25MB]                # keep originals; per-file limit
		templates      <dir>                       # override any template or style.css
		activity_import {                          # enables /import
			activity     Activity
			lap          Lap
			match        PlannedWorkout
			registration EventRegistration
			result       EventResult
		}
	}
}
```

The handler answers only the paths it owns (`/`, `/c/…`, `/calendar`, `/import`,
`/signup`, `/login`, `/logout`, `/me`, `/static/…`, `/api/v1/…`); everything else
falls through to the next handler, so `file_server`, `respond` or `reverse_proxy`
can serve the rest of the site. Behind a TLS-terminating proxy, set
`trusted_proxies` so sign-in rate limiting sees real client addresses.

MySQL/MariaDB DSNs use go-sql-driver syntax, e.g.
`user:password@unix(/run/mysqld/mysqld.sock)/dbname`. On start the database is
brought up to the schema without ever dropping anything: missing tables and columns
are created; type changes and removals are logged as needing a migration
(`ddframework diff --sql`).

## URL map

| Path | |
|---|---|
| `/c/<resource>` | list (`?q=`, `?mine=1`, `?<fk or enum>=<value>`, `?order=slot desc`, `?page=`) |
| `/c/<resource>?view=calendar&month=2026-11` | month calendar (classes with `calendar:`) |
| `/c/<resource>/new?<slot>=<value>` | create form, prefilled from the query string |
| `/c/<resource>/<id>`, `/edit`, `/delete` | detail, edit, delete |
| `/calendar` | the signed-in person's calendar classes on one grid (planner view) |
| `/import` | upload activity files |
| `/api/v1/<resource>[/<id>]` | JSON: GET, POST, PUT/PATCH, DELETE |
| `/api/v1/me` | who am I + the CSRF token for API writes |
| `/api/v1/import` | multipart upload (field `files`) |
| `/api/v1/openapi.json` | OpenAPI 3.0.3 |

`<resource>` is the class's table name with `-` for `_` (`planned-workouts`).

## Security model

- Passwords: bcrypt (`password` slot type). Sessions: random 256-bit tokens in an
  HttpOnly, SameSite=Lax cookie; only their SHA-256 is stored.
- CSRF: double-submit token on every form; API writes need the `X-CSRF-Token` header.
- Rate limits per client IP on sign-up, sign-in and uploads.
- Strict Content-Security-Policy (no scripts at all), `nosniff`, `frame-ancestors 'none'`.
- Private records answer 404, never 403, so their existence isn't revealed.
- Foreign keys are checked on every write: you can only link to rows you can read,
  and to *your own* rows of classes that have an owner (no attaching your result to
  someone else's activity, or your workout to someone else's plan).
- Upload size limits per file and per request; originals are stored under
  content-hash names, never user-supplied paths.

## Activity import conventions

`activity_import` names the classes uploads fill. Slots are matched by name and set
only when present — see `app/imports.go` for the list. Importers are pluggable:
implement `importer.Importer` and call `importer.Register` to support another
watch or timing format.

## Development

```sh
go test ./...                                                  # SQLite
DDF_TEST_MYSQL_DSN='user:pw@unix(/run/mysqld/mysqld.sock)/scratch' go test ./...
```

The MySQL run drops every table in the scratch database.

## License

Apache License 2.0 — see `LICENSE`.
