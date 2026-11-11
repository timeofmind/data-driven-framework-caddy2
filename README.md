# data-driven-framework-caddy2

An app-agnostic [Caddy v2](https://caddyserver.com) HTTP handler that runs
data-driven applications. It contains no application: an application lives in its
own repository and is loaded through the handler.

```
my-app/                      ← the application (its own repo)
  app.yaml                   name, menu, sign-up text, where the rest is
  model/*.yaml               the data model: structure, constraints, relationships,
                             access rules and behaviour (data-driven-framework-go)
  pages/*.yaml               the interface: one file per page, with its interface views
  theme/                     the look: any template to replace, static/style.css …
```

The data model and the page files are both defined — and checked — by
[data-driven-framework-go](https://github.com/timeofmind/data-driven-framework-go):
the model by its `schema` package, the pages by its `ux` package (see its
[interface model](https://github.com/timeofmind/data-driven-framework-go/blob/main/docs/interface-model.md)),
drawn as HTML by its `web-ux` package. This handler is the server around them:

- **Routing**: each page file's path is served (`ux.Site.Match`), its interface views
  built (`ux.Engine.Build`) and drawn (`web-ux`); a POST to a page carries out what
  one of its views writes (`ux.Engine.Submit`) after the CSRF check. Paths no page,
  account route or API claims fall through to the next handler.
- **Accounts** for the model's `auth` class: sign-up, sign-in, sessions, `/me` (the
  signed-in person's page: their class's `default_for` page), and account deletion
  (a detail view's delete on their own record ends every session).
- **The model's access rules**, applied to every page, list, choice and API call
  (by the framework).
- **The scripts** web-ux builds — the model's rules for forms, calendar drag, list
  reordering — at `/_dd/<name>.<hash>.js`, cached for good. The CSP allows only the
  site's own scripts (`script-src 'self'`).
- **Calendar feed**: a calendar view with `feed: true` gets `<its path>/feed`: each
  signed-in person's private iCalendar link (`<path>/feed/<token>.ics`) to subscribe to
  from Google Calendar ("From URL"), Apple Calendar or Outlook — that view's entries for
  them from six months back to eighteen ahead, as all-day events. Resetting the link
  cuts off the old one at once.
- **A JSON API** at `/api/v1/<resource>` for every class, with a generated OpenAPI 3
  document; `read: owner` fields only reach their owner.
- **Timed triggers**: the model's `at` triggers (a game's clock running out) are fired
  by the cluster's leader, or by the only node, through the normal save path; `timers off`
  in the `datadriven` block keeps a node from firing them.
- **File uploads** into an upload view's class (one with an `import` block): built-in
  readers for FIT (Garmin, COROS, Suunto, Wahoo, Polar), GPX, TCX and timing-system
  CSV produce device-neutral fields; the model's import block decides what they become.

## Build

```sh
go build -o caddy ./cmd/caddy      # standard Caddy + this handler
# or
xcaddy build --with github.com/timeofmind/data-driven-framework-caddy2
```

## Caddyfile

Only deployment settings go here; everything about the application is in its directory.

```caddyfile
example.com {
	datadriven [<base_path>] {
		app            /srv/my-app                    # the application directory
		database       mysql|sqlite <dsn>             # or:
		database_file  mysql|sqlite <file holding the DSN>
		admin          <login> [<login>...]           # e.g. emails with full access
		secure_cookies on|off                         # default on
		session_ttl    720h
		uploads        <dir> [25MB]                   # keep originals; per-file limit
	}
}
```

The handler answers its pages' paths, `/signup`, `/login`, `/logout`, `/me`, `/forgot`
and `/reset` (when `password_reset` names the app's class of reset codes: it belongs to
the auth class and has `code`, `expires_at` and `used_at`; the record the forgot page
saves flows wherever the routing sends the class — a mail listener with a template
holding `{{.base_url}}/reset?code={{.record.code}}` — and the answer never says whether
the account exists), a
calendar's `/feed`, `/static/…`, `/_dd/…` and `/api/v1/…`; everything else falls
through to the next handler.

## app.yaml

```yaml
name: "My App"
tagline: "One line under the name"
model: [model/*.yaml]              # globs, relative to the app directory (default)
pages: [pages/*.yaml]              # default
theme: theme                       # default "theme"
timezone: America/Vancouver        # "today", and the default for imports
signup: {enabled: true, intro: "Shown above the sign-up form"}
password_reset: {class: PasswordReset, expires: 1h}   # /forgot emails a link, /reset?code=… sets the password
nav: [calendar, walks, routes]     # page names in the main menu
```

Unknown keys are errors.

## Themes

A theme replaces any of web-ux's templates by file name — `layout.html` (defining
`head` and `foot`, the frame of every page), `page.html`, the interface views'
`iv-list.html`, `iv-calendar.html`, `iv-detail.html`, `iv-form.html`,
`iv-chips.html`, `iv-upload.html`, `iv-text.html`, and `confirm`, `error`, `login`,
`signup`, `forgot`, `reset`, `feed` — and supplies `static/` (served at `/static/…`). Anything it leaves
out comes from the built-in look. Calendar items and tags carry CSS classes for enum
values (`<slot>-<value>`, `tag-<value>`) so a theme can colour them.

## File readers

Uploaded files become these fields for the import block's expressions:
`sport, sub_sport, title, device, format, file_name, file_hash, start_utc,
start_local, date_local, elapsed_s, moving_s, distance_m, distance_km, ascent_m,
avg_hr, max_hr, calories, avg_cadence, avg_power_w, track_points`, and children
`laps` with `index, start_local, elapsed_s, moving_s, distance_m, distance_km,
ascent_m, avg_hr, max_hr`. Readers are pluggable (`importer.Register`).

## Security model

- Passwords: bcrypt. Sessions: random 256-bit tokens in HttpOnly, SameSite=Lax
  cookies; only their SHA-256 is stored.
- Changing an account's password takes the current one: on the profile form, and as
  `current_password` in an API `PATCH` (admins excepted).
- Admins (`admins` logins, and accounts whose model `auth: {admin: <slot>}` flag is set)
  bypass access rules and may set `write: admin` fields; an empty `admins` (e.g. an unset
  environment variable) grants nothing, and only an admin can set the flag.
- A record's party (`access: party` / `party_via`, e.g. a team's officers for a
  membership) may set only its `write: party` fields through the API, and create a row
  for someone else only where the model allows (`party_creates`); others' values for
  those fields are ignored, as in forms. Deletes follow `Registry.Deletable`.
- `/signup?<login>=…` fills in the login (an invitation's signup link). Rows shared with a team (`access: shared`) are listed and returned to the
  owner's groupmates only; values hidden by `private_when` are left out of responses.
- CSRF: double-submit token on every form; API writes need `X-CSRF-Token`
  (from `GET /api/v1/me`).
- Rate limits per client IP on sign-up, sign-in and uploads.
- Strict Content-Security-Policy (only the site's own scripts; no inline script, no
  eval), `nosniff`, `frame-ancestors 'none'`.
- Every page write is declared by its interface view and enforced: a form writes only
  the fields it lists.
- Private records answer 404. Foreign keys are checked on every write: only rows you
  can read, and your own rows of owned classes, can be linked.
- Upload size limits per file and per request; originals stored under content-hash names.

## Development

```sh
go test ./...                                                       # SQLite
DDF_TEST_MYSQL_DSN='user:pw@unix(/run/mysqld/mysqld.sock)/scratch' go test ./...
```

Tests run against `app/testdata/walkclub`, a small fixture application (model,
pages, theme). The MySQL
run drops every table in the scratch database.

## License

Apache License 2.0 — see `LICENSE`.
