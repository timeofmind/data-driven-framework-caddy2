# data-driven-framework-caddy2

An app-agnostic [Caddy v2](https://caddyserver.com) HTTP handler that runs
data-driven applications. It contains no application: an application lives in its
own repository and is loaded through the handler.

```
my-app/                      ← the application (its own repo)
  app.yaml                   name, navigation, home page, uploads, sign-up text
  schema/*.yaml              data structure, constraints, relationships, access rules
                             and behaviour, in data-driven-framework-go YAML
  theme/                     page layout and styling
    layout.html  home.html  list.html  detail.html  …
    static/style.css  …
```

From that directory the handler serves:

- **Pages and forms generated from the schema** — lists (search, filters, paging),
  month calendars, record pages with related records, create/edit forms with
  validation, delete confirmations. Forms, fields, tables and calendar grids are the
  handler's generic components; the app's theme places and styles them.
- **Accounts** for the schema's `auth` class: sign-up, sign-in, sessions, profiles,
  account deletion.
- **Access rules** from the schema's `access` blocks, applied to every page, list,
  relation picker and API call.
- **Declared behaviour** from the schema: computed fields, calendar labels, on-save
  triggers, ranked (leaderboard) relationships, add-button labels and conditions.
- **Shared libraries** from the schema's `filters`, `actions`, `usage` and belongs_to
  `link: readable`: lists get a choice of filter values (grouped, nested values
  matching their children, every chosen value required); a record found that way
  carries the search to its copy action and to "add" links that use it; each use is
  logged with the values searched for, and lists rank records by how many people chose
  them with the same values (an explicit `?order=` replaces the ranking). Actions live
  at `/c/<resource>/<id>/<action>` (GET confirms, POST runs).
- **Rules** from the schema (framework v0.5): select lists offer only the enum values
  an `options` rule allows and only the linked records a `match` allows, given the
  record's current values (including values reached through its links, such as the
  parent of a child form); enum `labels` and class `title` set the text shown; fields
  whose `when` does not hold are left off record pages. Every rule is enforced again on
  save, so forms stay correct without JavaScript.
- **Rules in the browser** (framework v0.6): when the schema has rules, the handler
  builds one script when the app loads — a small runtime (`app/rules.js`, no
  libraries) plus the schema's rules — and serves it at `/_dd/rules.<hash>.js` with a
  year-long immutable cache, so a schema change gives it a new name. As a form is
  filled in it hides fields whose `when` fails, switches `required` with
  `required_when`, and narrows select lists by `options` and `match` (options that
  don't fit are rendered `hidden disabled`, and come back when the field they depend on
  changes). The page's CSP allows only the site's own scripts (`script-src 'self'`;
  no inline script, no eval). Themes that override `form.html` add
  `{{template "rules" $d.Rules}}` inside the form; field wrappers need the
  `data-dd-field` attribute the built-in `field` component sets. The runtime is tested
  against the framework's expression conformance cases (`go test` runs it in goja).
- **Ordered lists**: children numbered by a `sequence` slot show the number as a column,
  and a new one goes at the end. Each row's own page offers "Move to position" (a POST
  to `<record>/move`) to anyone who may edit it; the rows in between close up or make
  room, and the parent's page is shown after. Themes that override `detail.html` add
  `{{template "move" $d.Move}}` among the record's actions.
- **Drag to reschedule**: on calendar pages, entries the viewer may edit whose class can
  move (`calendar` is the record's own writable date, and the class doesn't say
  `calendar_move: false`) can be dragged to another day. A small script
  (`/_dd/calendar.<hash>.js`, cached for good) posts the day to `<record>/reschedule`,
  which checks permission, moves the record (`calendar_end` by as many days) through the
  schema's rules and answers in JSON; the entry moves on the page once it is saved.
  Themes that override `calendar.html` add `data-dd-calendar data-dd-csrf="{{.CSRF}}"` to
  the grid, `data-date` to each day, the item attributes and the script tag (see the
  built-in template).
- **Calendar feed**: `/calendar/feed` gives each signed-in person a private iCalendar
  link (`/calendar/feed/<token>.ics`) to subscribe to from Google Calendar ("From URL"),
  Apple Calendar or Outlook: everything on their personal calendar from six months back
  to eighteen ahead, as all-day events (multi-day records span their days). Resetting
  the link cuts off the old one at once. Themes that override `calendar.html` can link
  to it with `$d.FeedURL`.
- **A JSON API** at `/api/v1/<resource>` with a generated OpenAPI 3 document.
- **File uploads** into a class with an `import` block: built-in readers for FIT
  (Garmin, COROS, Suunto, Wahoo, Polar), GPX, TCX and timing-system CSV produce
  device-neutral fields; the schema decides what they become.

The schema language (including behaviour and imports) is documented in
[data-driven-framework-go](https://github.com/timeofmind/data-driven-framework-go).

A proposed split of the interface out of the schema, into page files with
components (lists, calendars, forms, pickers), is in
[docs/interface-model.md](docs/interface-model.md).

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

The handler answers only its own paths (`/`, `/c/…`, `/calendar`, `/upload`,
`/signup`, `/login`, `/logout`, `/me`, `/static/…`, `/api/v1/…`); everything else
falls through to the next handler.

## app.yaml

```yaml
name: "My App"
tagline: "One line under the name on the home page"
schema: [schema/*.yaml]            # globs, relative to the app directory
theme: theme                       # default "theme"
timezone: America/Vancouver        # default for imports
signup: {enabled: true, intro: "Shown above the sign-up form"}
nav: [Meetup, Walk]                # default: every top-level class
home:
  - {class: Meetup, title: "Upcoming meetups"}       # upcoming first, else most recent
  - {class: Walk, mine: true, title: "Your walks"}   # only the signed-in person's
uploads:
  class: Walk                      # must have an import block in the schema
  title: "Upload GPS files"
  intro: "…"
  help: ["…"]
```

## Themes

A theme supplies `layout.html` (defining `layout`, which renders
`{{template "content" .}}`) and any of the page templates `home`, `list`,
`calendar`, `detail`, `form`, `confirm`, `login`, `signup`, `upload`, `error`, plus
files under `static/` (served at `/static/…`). Anything the theme leaves out comes
from the built-in default theme. Page templates use the components `table`, `field`,
`facets` (a list's filter choices, placed inside its search form) and the page data
documented in `app/views.go` and `app/pages.go`. Calendar items and tags carry CSS
classes for enum values (`<slot>-<value>`, `tag-<value>`) so a theme can colour them.

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
- CSRF: double-submit token on every form; API writes need `X-CSRF-Token`
  (from `GET /api/v1/me`).
- Rate limits per client IP on sign-up, sign-in and uploads.
- Strict Content-Security-Policy (no scripts), `nosniff`, `frame-ancestors 'none'`.
- Private records answer 404. Foreign keys are checked on every write: only rows you
  can read, and your own rows of owned classes, can be linked.
- Upload size limits per file and per request; originals stored under content-hash names.

## Development

```sh
go test ./...                                                       # SQLite
DDF_TEST_MYSQL_DSN='user:pw@unix(/run/mysqld/mysqld.sock)/scratch' go test ./...
```

Tests run against `app/testdata/walkclub`, a small fixture application. The MySQL
run drops every table in the scratch database.

## License

Apache License 2.0 — see `LICENSE`.
