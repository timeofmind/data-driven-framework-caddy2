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
  mobile/                    optional: templates that replace the theme's for a phone
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
  (a detail view's delete on their own record ends every session). `/signup` and
  `/login` take `next` (a path of the site, with or without the base path).
- **Guests**, when the auth class names a `guest` slot: a visitor who creates a row
  where the class says `access: guests` (a cart) is given a guest row (display "Guest")
  and a session on the spot — by a page's form or by `POST /api/v1/<class>`. Signing up
  fills that row in (the cart stays); signing in moves the guest's rows to the account
  (`runtime.Registry.Adopt`) and removes the guest. Guests are not signed in: `/me`,
  feeds and `access: signed_in` pages turn them away; `GET /api/v1/me` says `guest:
  true`.
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
- **A directory of listeners** at `GET /api/v1/_listeners`: the listener types the
  framework supports, each with its settings and which are secrets (`listener.Infos`),
  for an admin page offering the services an admin may supply a key for.
- **An admin backend in every app**, with the **Service Configuration** page at
  `/admin/services` (admins only; "Admin" in the default layout): the destinations the
  app's routing names — payments, mail, a bucket, a language model — each with the
  settings its listener type takes, drawn from the directory. What the routing file
  leaves out, above all the secrets, an admin supplies here; the values are kept in the
  framework class `FrameworkServiceSetting` (secrets sealed under `token_key_file`),
  the listener is opened again with them at once and at every start, and what was parked
  for it while it waited is delivered. A destination without all its settings no longer
  keeps the app from starting: it waits, parking what flows to it. See the framework's
  `docs/service-configuration.md`.
- **The admin area** is the same in every app and styled by its theme: Service
  Configuration, **Media** (`/admin/media`) and **Admins** (`/admin/admins`) are the
  server's three sections, and the app names its own in `admin.yaml` beside `app.yaml`
  (`sections: [{label, path, level}]`; without it, the app's pages under `/admin/`).
  **Admins** keeps the app's admins and their authority: the `admins` of the server's
  configuration are the owners (level 0, not changed in the app); others are added on
  the page by login with a level (1–99, lower is more authority), kept in the app's
  data (`FrameworkAdmin`), so a change holds on every node with no restart. An account
  that exists becomes an admin at once; anyone else is one after signing up through
  their sign-in link. **Admins sign in at a link of their own** (`/_dd/login?k=…`, made
  on the Admins page, mailed where the app has a mail listener): it asks for the
  password (or lets a new admin set one), is good for 1–365 days (30 by default)
  counted from the last sign-in through it, and `/login` refuses an admin's account.
  `caddy admin-link` remains the operator's way in when nobody has a link yet. A section or page that names a `level`
  is for admins whose own level is lower; an admin changes only admins of less
  authority than their own. See "Admins and their levels" in the framework's
  `docs/interface-model.md`. The layout draws the admin navigation on
  those pages for admins (a theme with its own `layout.html` renders `.AdminNav`). The
  Media page is the app's media library on the hosting: uploads (drag and drop), the
  grid with each file's address, alt text, delete. Its calls go to `/_dd/media/…` on this
  server — admin session and `X-CSRF-Token` — which relays them to `dd-media` with the
  node's token: `DD_MEDIA_URL` and `DD_MEDIA_TOKEN` in the process environment (the
  hosting gives every node both; `Config.MediaURL`/`MediaToken` in Go). A slot is
  answered with an `upload_token` (the presigned POST sealed under `token_key_file`, so
  any node of the cluster may take the file) and the file is streamed through to the
  bucket, never kept here. Without a token the page says the library is not available.
  See the framework's `docs/interface-model.md`, "The admin area".
- **A notice when saved changes are not being stored**: a destination that keeps
  refusing an entry of the change log (a database that will not take a row) is shown
  to admins on every page of the admin area once it has gone on for a minute — which
  destination, the change, the reason, how many changes wait behind it — with any
  changes that were set aside (parked). Until then it was only in the server's log.
- **Sign-in providers** (`/admin/sign-in`, the Sign-in section of the admin area, for
  the admins who may see keys): an owner lets people sign in with an account they have
  elsewhere — Google, or any OpenID Connect provider — as well as with a password. An
  app has the page when its routing names a `signin` listener (`signin: {type:
  signin}`), the way a service is on the Service Configuration page when the routing
  names its listener; an app without one has no page and offers no provider.
  The page then shows the redirect address to
  register with the provider (`<the app's address>/_dd/signin/<provider>/callback`) and takes
  the client ID and secret, kept sealed with the service settings. Once a provider is
  switched on, the sign-in and sign-up pages offer "Continue with …". Accounts are matched by the email the provider says it verified;
  an unverified one is refused. Someone new gets an account where the app takes
  sign-ups; an account that already existed keeps its password, which is asked for
  once before the provider is joined to it (the app never checked who made the
  account), and after that its holder chooses either way at every sign-in. Admins are refused:
  they use their own sign-in links. The server calls a provider over https only, and
  never an address inside its own network.
- **The admin link** for an app's first admin: `caddy admin-link --token-key FILE
  --login EMAIL --base-url URL [--ttl 10m]` (or `app.AdminLink`) mints
  `/_dd/admin?token=…`, signed with the token key and naming one of the `admins`
  logins; opening it signs that account in, or leads to sign-up with the login fixed —
  allowed even when sign-up is off — and on to Service Configuration. Good for ten
  minutes by default, on any node, not single-use.
- **A JSON API** at `/api/v1/<resource>` for every class, with a generated OpenAPI 3
  document; `read: owner` fields only reach their owner.
- **View indexes and kept values** (framework `docs/searches-at-scale.md`) are settled
  on start: on SQLite before serving, otherwise in the background, lists reading
  through their join classes until then. A node that can't settle — in a rolling
  upgrade, the first node restarted, while the others still run a version without the
  new index and refuse its rows — tries again (after 30 s, doubling to every 10 min)
  until it does.
- **Timed triggers**: the model's `at` triggers (a game's clock running out) are fired
  by the cluster's leader, or by the only node, through the normal save path; `timers off`
  in the `datadriven` block keeps a node from firing them.
- **Records from answers**: an `llm` template with `json: true` and `import: {class, link}`
  has its answer made into a record of that class (its import block: fields, children and
  their children — a workout's sets and intervals), owned by the asking record's owner,
  the new id written onto the asking record with its answered status, in one write; all
  or nothing. A refused write is retried with the kept answer, never by asking again.
- **Lists of a relationship**: a page's list `of:` a has_many shows what the relationship's
  `count` counts — its foreign key or `match`, and its `where` (framework v0.50.30).
- **Language models**: a class routed to an `llm` listener (framework `listener/llm`)
  has each new record rendered through a template in the app's `llm/` directory (or a
  record of the listener's `template_class`) and asked of the configured providers in
  priority order — Claude, OpenAI, Kimi — until one answers; the answer is saved onto
  the record through the normal save path, so it is pushed to pages and the model's
  triggers carry it on. The cluster's leader asks; API keys come from the environment.
- **Apps that build apps**: a class routed to an `appbuilder` listener (framework
  `listener/appbuilder`) has each new record's description sent to the dd-app-builder
  API; the entry is confirmed as soon as the API has it, and the handler starts the
  listener's loop, which follows the build on the cluster's leader (the `Find` and
  `Leading` hooks find builds under way after a restart) and saves the phase, the
  progress and the preview's address onto the record through the normal save path.
- **Records as files in a bucket**: a class routed to an `s3` listener (framework
  `listener/s3`) has each record put into an S3 bucket — Amazon's or a compatible store —
  as JSON, as one slot's value, or rendered through a template in the app's `s3/`
  directory (resolved and checked at start like the mail templates); where the object
  went is saved onto the record.
- **DNS records at Cloudflare**: a class routed to a `cloudflare` listener (framework
  `listener/cloudflare`) has each record kept as a DNS record in the zone named by `zone`;
  the zone's API token (Zone · DNS · Edit) is given by an admin on Service Configuration,
  and until then what flows to it is parked. The outcome (`status`, `record_id`) is
  saved onto the record.
- **Apps that read mail**: a class routed to an `imap` listener (framework
  `listener/imap`, `on: [update, delete]`) holds one record per message of an account.
  The handler starts the listener's loop, which looks at the mail server on the
  cluster's leader and saves new mail and changed flags through the normal save path
  (`Upsert`), and deletes the record of a message that is gone (`Remove`). Saving a
  record's `mailbox` or flags, or deleting it, moves, flags or deletes the message.
- **Read your writes across the nodes**: every answer sets a `dd_seen` cookie with the
  newest change this node had applied, and every request waits (briefly) for the version
  the browser carries — so a page or API read a load balancer sends to the other node is
  never older than the browser's last write; the JSON API also takes `_seen` and answers
  `_version` / `X-DD-Version`.
- **Shops**: the framework's `shipping` and `paypal` listeners (a test mode that charges
  nothing), receipts by mail on an update, and the cart library served at `/_dd/cart.js`
  are wired here like mail: any listener taking hooks gets the app's records, their
  children and the write-back. See the framework's `docs/shop-library.md`.
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
		token_key_file <file>                         # signs form tokens and admin links,
		                                              # seals the services' secrets; the
		                                              # same file on every node
	}
}
```

From the environment: `DD_MEDIA_URL` and `DD_MEDIA_TOKEN` (the hosting's media library
for the Media page; absent on a self-hosted app), and `DD_MEDIA_PUBLIC`, where the
library's files are served from: its origin is added to the pages' `img-src`, so a
picture the image listener drew or an admin uploaded shows (without it the browser
refuses any picture not on the app's own origin or in `image_hosts`).

The handler answers its pages' paths, `/signup`, `/login`, `/logout`, `/me`, `/forgot`
and `/reset` (every app whose members sign in with a password, with nothing to declare:
the forgot page mails a link through the app's mail listener — an `smtp` listener, which
dd-app-builder sets up on its relay when it publishes the app — signed with the token key,
good for `password_reset.expires` (an hour) and only until the password changes, so it
works once; without a configured mail listener, as in a preview, the forgot page says the
app can't send mail yet; the answer never says whether the account exists. An app may
instead keep its own class of reset codes, `password_reset: {class: …}`: it belongs to
the auth class and has `code`, `expires_at` and `used_at`, and flows wherever the routing
sends it — a mail listener with a template holding
`{{.base_url}}/reset?code={{.record.code}}`), a
calendar's `/feed`, `/admin/services`, `/admin/media`, `/static/…`, `/_dd/…` (scripts,
`/_dd/admin` for an admin link, `/_dd/media/…` for the Media page) and `/api/v1/…`;
everything else falls through to the next handler. A page file may not claim
`/admin/services` or `/admin/media`.

## app.yaml

```yaml
name: "My App"
tagline: "One line under the name"
model: [model/*.yaml]              # globs, relative to the app directory (default)
pages: [pages/*.yaml]              # default
theme: theme                       # default "theme"
mobile: mobile                     # default "mobile": templates for a phone (see Themes)
timezone: America/Vancouver        # "today", and the default for imports
signup: {enabled: true, intro: "Shown above the sign-up form"}
password_reset: {expires: 1h}      # optional: how long the mailed reset link works (class: an app's own reset codes)
image_hosts: [https://my-media.s3.us-east-1.amazonaws.com]   # origins pages may load pictures from besides the app's own (the CSP)
nav: [calendar, walks, routes]     # page names in the main menu
```

Unknown keys are errors.

## Themes

A theme replaces any of web-ux's templates by file name — `layout.html` (defining
`head` and `foot`, the frame of every page), `page.html`, the interface views'
`iv-list.html`, `iv-calendar.html`, `iv-detail.html`, `iv-form.html`,
`iv-chips.html`, `iv-upload.html`, `iv-text.html`, and `confirm`, `error`, `login`,
`signup`, `forgot`, `reset`, `feed` — and supplies `static/` (served at `/static/…`). Anything it leaves
out comes from the built-in look. An app's `mobile/` replaces any of those templates again
for a phone (its User-Agent, or `?dd_view=mobile|desktop`, kept in a cookie): the same
pages at the same addresses, drawn for a small screen (framework
`docs/interface-model.md`, "Mobile templates"). Calendar items and tags carry CSS classes for enum
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
- `/signup?<login>=…` fills in the login (an invitation's signup link); with a valid
  admin link token in `_admin` the login is the link's and sign-up is allowed even when
  off. An admin link is an HMAC of the token key naming the login and its expiry, refused
  for a login not in `admins`.
- The services' secrets (`/admin/services`) are sealed with AES-GCM under a key derived
  from `token_key_file` before they are stored, and never shown again; without a token
  key file the save is refused. Rows shared with a team (`access: shared`) are listed and returned to the
  owner's groupmates only; values hidden by `private_when` are left out of responses.
- CSRF: double-submit token on every form; API writes need `X-CSRF-Token`
  (from `GET /api/v1/me`).
- Rate limits per client IP on sign-up, sign-in, uploads, new guests and guests' writes
  (`app.yaml` `guests: {per_hour: 20, writes_per_hour: 60, display: Guest, keep_for: 30d}`;
  idle guests are swept after `keep_for` with what they own).
- Every API answer carries `X-CSRF-Token`; a visitor becoming a guest keeps their token.
- `GET/POST/DELETE /api/v1/_parked` (admins): entries a destination gave up on — list,
  replay one after mending what refused it, or drop it; `_stats` counts them.
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
