# The interface model: pages and interface views

Status: **proposed** — nothing here is implemented yet. For review before any code
changes.

## Why

An application is described today by its schema (data-driven-framework-go YAML) and a
few lists in `app.yaml`. The schema has grown to carry two different things:

- **The data**: types, constraints, relationships, access rules, derived values,
  what a date or a position means. These hold however a record is written — through
  a page, the JSON API, an upload or a trigger.
- **The interface**: field order (`ui_order`), groups, placeholders, which fields are
  hidden, list sorting, search, filters, button labels, which classes appear on a
  calendar and whether entries can be dragged there.

The interface half is in the wrong place. One class can only be shown one way; two
pages can't list the same records differently; a page that brings data from several
classes together (a calendar, or a form with helper data to fill it in) has nowhere to
be described, so the handler hard-codes it (`/calendar` gathers "every owned class with
a calendar date"). And some interface keys hide data rules: `add_when` only hides the
"Register" button, so the API still accepts a registration for a closed event.

The aim is for the model to say as much as it can about the data — far more than a
database schema does — without saying how the application presents the operations that
read, add to or change it. A calendar and an ordered list you can rearrange are two such
presentations: different views, for different purposes, of data whose meaning and rules
stay the same. Many pages can work on the same data, each from its own angle.

This document splits the two:

```
my-app/
  app.yaml          name, sign-up, menu, where the rest is
  model/*.yaml      the data model (today's schema/, minus the interface keys)
  pages/*.yaml      one file per page: its URL, who may see it, its interface views
  theme/            HTML: the page layout and one template per interface view type
```

- The **framework** reads the model only. It knows nothing about pages.
- The **handler** (this repository) reads the pages and draws them. It stays
  app-agnostic: page files name interface view types it provides, never app code.
- **Layout and look** stay in the theme's HTML templates. Page files say *what* is on
  a page and how it reads and writes data, not where it sits on the screen.

### The dividing line

> If a rule must still hold for a record written through the API with no page
> involved, it belongs to the **model**. If it is about how a person sees, finds,
> arranges or enters data, it belongs to a **page**.

So: the numbering of a workout's sets 1…n is model (the order is part of the workout,
and moving a set must keep the numbers gapless). Showing sets in that order, and
offering drag handles or a "Move to position" form to change it, is page. A
competition's start and end dates are model; showing competitions on a month grid, and
whether entries there can be dragged, is page.

### Why YAML

Page files are trees of named settings, not documents with mixed text and markup, so
the format is the model's: YAML, read by one loader and checked by one validator.
XML's advantages — mixed content, namespaces, XSD — would matter if page files held
the layout itself (interface views placed inside HTML); here the layout is the theme's
HTML. To keep YAML's weak spots (indentation, `no` read as `false`) from failing
silently, the page loader rejects unknown keys and wrong types, with file and line.
(The model parser should do the same; today it ignores keys it doesn't know.)

## app.yaml

```yaml
name: "Athletics Together"
tagline: "…"
model: [model/*.yaml]            # was: schema
pages: [pages/*.yaml]            # default
theme: theme
timezone: America/Vancouver
signup: {enabled: true, intro: "…"}
nav: [calendar, plans, workouts, activities, competitions, events]   # page names
```

`home` and `uploads` move to page files (`pages/home.yaml`, `pages/upload.yaml`).

## Page files

A page's name is its file name: `pages/workout.yaml` is the page `workout`.

```yaml
# pages/workout.yaml
path: /c/workouts/{id}           # URL; {id} and other {params} are path parameters
title: "record.title"            # expression; the page's heading and <title>
access: public                   # public | signed_in | an expression (see below)
record: {class: Workout, id: id} # optional: the record the page is about
params:                          # optional: query parameters the page reads
  month: {type: month}
  tag: {type: integer, many: true}
template: workout.html           # optional: a theme template that places the
                                 # interface views itself; default: page.html, which
                                 # renders them in order
interface_views:
  - …
```

- **`path`** keeps today's URLs (`/c/<resource>/…`, `/calendar`) so links and
  bookmarks keep working. Two pages may not claim the same path.
- **`record`**: the page loads that record, answers 404 if the viewer may not read it
  (as today), and interface views can refer to it as `record`.
- **`access`** only narrows. The model's access rules always apply as well: a page
  marked `public` still shows nobody a private record.
- **Pages not declared** are generated from the model as today (list, record, new,
  edit, delete for each class), so an app works with no page files and can move to
  them one page at a time. `ddcaddy generate-pages` writes the generated pages out as
  files to start from.

### Expressions

Page files use the framework's expression language (the one in `when`, `checks`,
`calendar_label`), with these names available:

| name | value |
|---|---|
| `viewer` | the signed-in person's record (the model's `auth` class), or `null` |
| `record` | the page's record, if it has one |
| `params.<name>` | path and query parameters |
| `today` | today's date in the app's time zone |
| `row` | in per-row settings (list columns, calendar text): the row being drawn |

### Links between pages

Anywhere a page links to another:

```yaml
link: {page: workout, id: row.id}                       # path parameter
link: {page: new-scheduled-workout, set: {date: day}}   # prefill a form there
```

The validator checks that the page exists and that every parameter it needs is given.

## The catalogue: standard ways to show and change data

The point of the split is a **standard, finite set** of ways data is presented and
manipulated, the same in every app, each one familiar from everyday software. An app
doesn't invent interactions; it picks from the catalogue and points each choice at its
model. Each entry is defined by three things:

1. **What it needs from the model**: the data shape that makes it meaningful. A
   calendar needs records that occur on dates; a reorderable list needs a sequence.
   The validator refuses a page that uses one on data without that shape.
2. **How it shows the data.**
3. **The manipulations it offers**, each mapped to exactly one kind of model write.

### Manipulations

Every way a person can change data through a page is one of these. Each has a
familiar gesture and, always, a plain form equivalent — for keyboards, touch screens,
screen readers and browsers without scripts — that makes the same write.

| manipulation | gesture | form equivalent | model write |
|---|---|---|---|
| **create** | "Add", "+" on a day, "Add" under a parent | the create form | insert, with the starting values the place implies (the day, the parent) |
| **edit** | "Edit"; later, click a value to edit it in place | the edit form | update of the listed fields |
| **delete** | "Delete" | confirmation page | delete (the model's `on_delete` decides what follows) |
| **reorder** | drag a row up or down | "Move to position" | the `sequence` slot, via `MoveTo` (the rows between shift, in one transaction) |
| **reschedule** | drag an entry to another day | the edit form's date | the `occurs` slot(s), an end moving with its start |
| **regroup** | drag a card to another column | the edit form's field | the enum or belongs_to slot the columns are made from |
| **reparent** | drag a node under another | the edit form's parent | the self-referencing belongs_to slot |
| **link / unlink** | add or remove a chip (a tag) | the join record's form | insert or delete a join row (many-to-many) |
| **copy / other action** | the action's button | confirmation page | the model `action` |

Whatever the gesture, the outcome is the same: the server applies the model's rules,
the page changes only once the write is saved, and a refusal shows the model's own
message (`must be on or after the start`) in place.

### Presentations

| interface view | needs from the model | shows | offers |
|---|---|---|---|
| `list` | any class | rows as a table, with search, sorting, filters, paging | create, delete; **reorder** if it orders by a `sequence`; `rank` numbering |
| `detail` | one record | its fields, grouped; its children as lists | edit, delete, actions |
| `form` | a class (create) or a record (edit) | its fields as inputs, with the model's rules live | create, edit |
| `calendar` | `occurs` | a month grid; multi-day records on each day | create on a day, **reschedule** |
| `chips` | a many-to-many (a join class) | the linked records as removable chips | **link / unlink** |
| `picker` | any class | a searchable, filterable list or calendar | fills fields of a form on the page |
| `upload` | an `import` block | a file drop | create (through the import) |
| `text` | — | text | — |
| *later:* `board` | an enum or belongs_to to make columns | cards in columns (e.g. by status) | **regroup**; reorder within a column if sequenced |
| *later:* `tree` | a self-referencing belongs_to (`Tag.parent_id`) | nested records | **reparent** |
| *later:* `timeline` | `occurs` | records as bars on a time axis | reschedule; change the end by dragging it |
| *later:* `chart` | numbers, and `occurs` or an order | values over time or by category | — |
| *later:* `map` | coordinates | records as places | — |

Because each entry's needs are model features, the handler can also choose sensibly
when a page isn't declared: a class that occurs on dates gets a calendar beside its
list, a sequenced child list gets reorder, a many-to-many gets chips. Declared pages
choose for themselves.

New presentations and manipulations are added to the catalogue in this repository —
generic, with their model needs, their write and their form equivalent — never in an
app.

## Interface views

A page is a list of **interface views**, each an entry from the catalogue. An interface
view reads data from the model, shows it in some form, and may write data back.

The name is deliberately specific. A **DataView** — planned as a deeper layer of the
framework, defined in the model — is a different thing: a data set derived from the
model's classes. Interface views belong to pages and are about presentation and
manipulation; they may later read from data views, but never define them.

Every interface view has:

| key | |
|---|---|
| `type` | `list`, `calendar`, `detail`, `form`, `chips`, `picker`, `upload`, `text` |
| `name` | unique on the page; needed when another interface view or a template refers to it |
| `title`, `intro` | heading and a line of text above it |
| `when` | expression: the interface view is shown only when it holds |

### Reading data

```yaml
data:
  class: PlannedWorkout                     # or:
  of: sets                                  # a has_many of the page's record
  where: "athlete_id == viewer.id"          # expression over the rows; narrows only
  order: "date asc"                         # default order
  limit: 50
```

The handler always applies the model's access rules first; `where` can only narrow
what the viewer may read, never widen it.

### Showing fields

A field is a slot, a computed value, or a path through belongs_to links
(`workout.name`, `competition.start_date`):

```yaml
fields:
  - date
  - workout.name
  - {field: target_distance_km, label: "km", format: "number:1"}
  - {field: status, as: badge}
  - {group: "Targets", fields: [target_distance_km, target_duration]}
```

Labels and descriptions come from the model (what the field *means*); an interface view may
override the wording (`label:`) without touching the model. `placeholder`, `help`
and `as` (the control or display: `select`, `radio`, `textarea`, `badge`, …) are
page settings.

### Writing data

Every write goes through the model: access rules, checks, sequences, `on_change`,
triggers and usage logging all run exactly as they do for the API. An interface view can
make writes *possible*; it can never make one *allowed* that the model refuses.

What an interface view may write is declared, and the server enforces the declaration:

- a **form** writes only the fields it lists, plus its `set:` values (today a form
  accepts every writable slot of the class);
- a **list** with `reorder` writes only the sequence slot it names;
- a **calendar** with `drag` writes only the date slot(s) the model's `occurs` names;
- **chips** create and delete only rows of their join class;
- **actions** run model actions (`copy`) or delete.

Writes are POSTed to the page's own URL with `_view=<name>` (and `_record=<id>`
for a row), plus the CSRF token. The handler looks up that interface view's declaration on
that page and applies only what it allows. Scripts (drag, reorder) post the same way
and get JSON back. Same-origin scripts only, as now (`script-src 'self'`).

### `list`

A table of rows.

```yaml
- type: list
  name: workouts
  data: {class: Workout, order: "name asc"}
  fields: [name, sport, sub_activity_id, surface, author_id]
  link: {page: workout, id: row.id}       # default: the class's record page
  search: [name]                          # free-text search over these fields; omit for none
  sort: ["name asc", "created_at desc"]   # orders the viewer may switch between
  filters:
    - {name: tag, label: "Tags", via: tags, slot: tag_id, include_children: parent_id,
       group_by: category_id, collapse_groups: true, rank_by_usage: true}
  mine: true                              # offer "Only mine" (needs an owner)
  page_size: 25
  add: {page: new-workout, label: "New workout"}
```

For lists of a record's children (`data: {of: sets}`):

```yaml
  reorder: {field: position, with: drag}  # or: move_to_position (a form per row)
  rank: {best_per: athlete_id}            # leaderboard: number the rows; keep each
                                          # athlete's best (first in `order`)
  add: {page: new-registration, label: "Register", set: {event_id: record.id}}
```

`reorder` needs the field to be a `sequence` in the model; moving a row uses the
model's `MoveTo`, which shifts the rows in between in one transaction.

### `calendar`

A month grid of rows from one or more classes.

```yaml
- type: calendar
  name: month
  month: params.month                     # default: this month
  show:
    - class: PlannedWorkout
      where: "athlete_id == viewer.id"
      text: "title + ' · ' + round(target_distance_km, 1) + ' km'"
      colour_by: status                   # CSS class status-<value>
      drag: true                          # move to another day
    - {class: Activity, where: "athlete_id == viewer.id", text: "title + ' · ' + round(distance_km, 1) + ' km'"}
    - {class: CompetitionEntry, where: "athlete_id == viewer.id"}
  add: {page: new-scheduled-workout, set: {date: day}}   # "+" on each day
  feed: true                              # offer the private iCalendar link for these rows
```

Which date(s) a row sits on comes from the model (`occurs`, below), so every calendar,
and the feed, agree. `drag` is offered only when the date is the row's own slot (not a
path through a link) and the viewer may edit the row; a multi-day row moves its end by
the same number of days. `text` defaults to the record's title.

### `detail`

The fields of one record, and what can be done with it.

```yaml
- type: detail
  data: {record: record}
  fields: [name, sport, sub_activity_id, surface, description, cloned_from_id, visibility]
  actions:
    - edit                                # → the record's edit page
    - delete
    - {action: copy, label: "Copy to my workouts"}   # a model action
    - move_to_position                    # for rows of an ordered list
```

Fields whose model `when` does not hold are left out, as today.

### `form`

Creates or edits one record.

```yaml
- type: form
  name: workout
  creates: Workout                        # or: edits: record
  fields:
    - name
    - sport
    - sub_activity_id
    - {field: surface, placeholder: "Same for every interval"}
    - {group: "About", fields: [description, visibility]}
  prefill: {sport: params.sport}          # starting values (the viewer can change them)
  set: {}                                 # values the viewer can't change
  submit: "Save"
  then: {page: workout, id: saved.id}     # where to go after saving
```

The model's rules still drive the form: `options` and `match` narrow select lists,
`when` and `required_when` hide fields and switch `required`, `default_expr` fills
starting values — in the browser as the form is filled in, and again on the server.

### `chips`

The records linked to the page's record through a many-to-many, as chips: × on a chip
deletes its join row, and adding one creates a join row (choosing from the records the
model's `link` and `match` allow).

```yaml
- type: chips
  title: "Tags"
  data: {of: tags}                        # the has_many to the join class
  link: tag_id                            # the join class's FK to what is shown
  group_by: category_id                   # optional
```

### `picker`

Helper data for filling in a form: a list (or calendar) whose rows, when chosen, set
fields of a form on the same page. Nothing is written until the form is saved.

```yaml
- type: picker
  name: library
  data: {class: Workout, where: "visibility == 'public' or author_id == viewer.id"}
  fields: [name, sport, surface]
  filters: [{name: tag, via: tags, slot: tag_id, include_children: parent_id,
             group_by: category_id, collapse_groups: true, rank_by_usage: true}]
  fills:
    form: scheduled
    set: {workout_id: row.id, title: row.name, sport: row.sport}
```

When the form is saved, the picker's chosen filter values go with it, so the model's
`usage` log records which tags the workout was found with (today this is carried in
the URL as `_search`).

### `upload`

Files into a class whose model has an `import` block.

```yaml
- type: upload
  class: Activity
  help:
    - "Garmin Connect: open an activity → ⚙ → Export Original …"
```

### `text`

A paragraph or list of text: introductions, help.

## Changes to the model

Keys that leave the model, and where they go:

| model key today | becomes |
|---|---|
| slot `ui_order`, `ui_group`, `placeholder`, `control_type` | interface view `fields` (order, `group`, `placeholder`, `as`) |
| slot `hidden` | not listed in any interface view's `fields`; where it means *private* (`email`), a model read rule on the slot (`read: owner`) — new |
| class `order_by`, has_many `order_by` | interface view `data.order` |
| class `search: false` | interface view `search` (none unless listed) |
| class `filters` | list / picker `filters` |
| has_many `add_label` | list `add.label` |
| has_many `add_when` | a model rule on the child, checked on save (new: `create_when`) — e.g. EventRegistration: `event.registration_open`; WorkoutSet: the workout's sport has sets |
| has_many `rank` | list `rank` |
| class `calendar`, `calendar_end` | model `occurs` (below) + calendar `show` |
| class `calendar_label` | calendar `show[].text` |
| class `personal_calendar: false` | gone: a calendar lists the classes it shows |
| class `calendar_move: false` | gone: calendar `show[].drag` (off unless set) |
| usage `filter` | gone: the picker or list that found the record supplies the values |
| app.yaml `nav` (class names) | page names |
| app.yaml `home`, `uploads` | `pages/home.yaml`, `pages/upload.yaml` |

New in the model:

- **`occurs`**: what dates a record happens on, as data:

  ```yaml
  PlannedWorkout:   {occurs: {on: date}}
  Competition:      {occurs: {from: start_date, to: end_date}}       # to: blank = one day
  CompetitionEntry: {occurs: {from: competition.start_date, to: competition.end_date}}
  ```

  Calendars and the iCalendar feed place rows by it; drag writes it.
- **`create_when`** on a class: an expression that must hold for a new record to be
  saved (reads its links, e.g. `event.registration_open`). Replaces `add_when`, and is
  enforced for the API too.
- **Slot read rules**: `read: owner` on a slot keeps it out of what others can read
  (pages and API), replacing `hidden` where it stood for privacy.
- **Unknown keys are errors**, as in page files.

What stays in the model, because it holds whatever writes the data: types, `required`,
`min`/`max`/`pattern`, defaults and `default_expr`, `when`/`required_when`/`options`
(a field that does not apply is cleared on save), `sequence`, relationships with
`on_delete`, `link` and `match`, constraints, `access`, `auth`, `checks`, `computed`,
`triggers`, `on_change`, `import`, `actions` (what a copy does), `usage` (what counts as
a use, and where it is logged), `display`/`title`, `label`/`label_when`/`description`,
and enum `labels`. `computed` keeps `format`/`suffix` as the default way to show the
value; an interface view can override it.

## Validation

`make check` (and the handler at start-up) loads the model and every page, and fails
with file and line when:

- a key is unknown or has the wrong type;
- a class, field, path, relationship, filter or action named by a page does not exist;
- an expression does not parse or names something that does not exist;
- a link names a page that does not exist or leaves out a parameter it needs;
- a `reorder` field is not a `sequence`, a `drag` class has no `occurs`, a form lists
  a field the model makes read-only, or an `upload` class has no `import`;
- two pages claim the same path.

## Theme

`page.html` renders a page's interface views in order. Each interface view type has its own
template (`interface-views/list.html`, `interface-views/calendar.html`, …), built in and
overridable by the theme. A page that needs its own arrangement names a template that
places interface views by name: `{{interfaceView "month"}}`, `{{interfaceView "library"}}`.

## Worked examples (athleticstogether.com)

### The personal calendar

```yaml
# pages/calendar.yaml
path: /calendar
title: "'Your calendar'"
access: signed_in
params: {month: {type: month}}
interface_views:
  - type: calendar
    name: month
    month: params.month
    show:
      - class: PlannedWorkout
        where: "athlete_id == viewer.id"
        text: "title + ' · ' + round(target_distance_km, 1) + ' km'"
        colour_by: status
        drag: true
      - class: Activity
        where: "athlete_id == viewer.id"
        text: "title + ' · ' + round(distance_km, 1) + ' km'"
      - class: CompetitionEntry
        where: "athlete_id == viewer.id"
        text: "competition.name + if(events != null, ' · ' + events, '')"
      - class: EventRegistration
        where: "athlete_id == viewer.id"
        text: "event.name + ' (virtual)'"
    add: {page: new-scheduled-workout, set: {date: day}}
    feed: true
```

Compared with today: the four classes, their text, which one "+" creates and which can
be dragged are all written here instead of being worked out from `calendar`,
`personal_calendar`, `calendar_move` and `calendar_label` spread across the model.

### A workout, with its sets in order

```yaml
# pages/workout.yaml
path: /c/workouts/{id}
title: "record.name"
access: public
record: {class: Workout, id: id}
interface_views:
  - type: detail
    data: {record: record}
    fields: [name, sport, sub_activity_id, surface, description, cloned_from_id, visibility]
    actions: [edit, delete, {action: copy, label: "Copy to my workouts"}]

  - type: list
    name: sets
    title: "Sets"
    data: {of: sets, order: "position asc"}
    fields: [position, name, repeats, rest_after]
    reorder: {field: position, with: drag}
    add: {page: new-set, label: "Add set", set: {workout_id: record.id}}

  - type: chips
    title: "Tags"
    data: {of: tags}
    link: tag_id

  - type: list
    title: "Scheduled"
    data: {of: schedulings, order: "date desc"}
    fields: [date, title, status]
    add: {page: new-scheduled-workout, label: "Schedule this workout", set: {workout_id: record.id}}

  - type: list
    title: "Copies of this workout"
    data: {of: clones}
    fields: [name, author_id]
```

Whether a set can be added at all ("sets only for run, swim, strength …") is a
`create_when` on WorkoutSet in the model, so the button disappears and the API refuses
it for the same reason.

### Scheduling a workout, with the library as helper data

```yaml
# pages/new-scheduled-workout.yaml
path: /c/planned-workouts/new
title: "'Schedule a workout'"
access: signed_in
params:
  date: {type: date}
  workout_id: {type: integer}
interface_views:
  - type: picker
    name: library
    title: "Find a workout"
    data:
      class: Workout
      where: "visibility == 'public' or author_id == viewer.id"
    fields: [name, sport, surface]
    filters:
      - {name: tag, label: "Tags", via: tags, slot: tag_id, include_children: parent_id,
         group_by: category_id, collapse_groups: true, rank_by_usage: true}
    fills:
      form: scheduled
      set: {workout_id: row.id, title: row.name, sport: row.sport}

  - type: form
    name: scheduled
    creates: PlannedWorkout
    fields:
      - plan_id
      - date
      - workout_id
      - {field: title, placeholder: "e.g. Hill sprints"}
      - sport
      - {group: "Targets", fields: [target_distance_km, target_duration]}
      - description
    prefill: {date: params.date, workout_id: params.workout_id}
    set: {status: "'planned'"}
    then: {page: calendar, set: {month: saved.date}}
```

Today this page is the generic "new PlannedWorkout" form; finding the workout happens
on the library page first, and the tags searched with travel along in the URL. Here the
page itself brings the library in as helper data, and only the form writes.

### A virtual event and its leaderboard

```yaml
# pages/event.yaml
path: /c/events/{id}
title: "record.name"
access: public
record: {class: Event, id: id}
interface_views:
  - type: detail
    data: {record: record}
    fields: [name, sport, distance_km, start_date, end_date, description, registration_open]
    actions: [edit, delete]
  - type: list
    title: "Leaderboard"
    data: {of: results, order: "elapsed_time asc", limit: 1000}
    fields: [athlete_id, elapsed_time, distance_km, created_at]
    rank: {best_per: athlete_id}
  - type: list
    title: "Registered athletes"
    data: {of: registrations, order: "created_at asc"}
    fields: [athlete_id, team]
    add: {page: new-registration, label: "Register", set: {event_id: record.id}}
```

Registration closing is `create_when: "event.registration_open"` on EventRegistration
in the model; the page just offers "Register", and the button is left out whenever the
model would refuse the record.

## Moving over

1. **Framework**: add `occurs`, `create_when` and slot read rules; reject unknown
   keys. Old keys keep working (and keep driving the generated pages) until step 4.
2. **Handler**: load `pages/*.yaml`; implement the interface view types; route declared
   pages, generate the rest; `generate-pages`. Tests against the walkclub fixture with
   a mix of declared and generated pages.
3. **athleticstogether**: rename `schema/` to `model/`, add `occurs` and `create_when`;
   write the calendar page first, then the workout, library, scheduling and event
   pages, then the rest, checking each live.
4. **Framework**: once no page relies on them, drop the interface keys from the model
   (a major version), and the generated pages read field order from declaration order.

## Open questions

- **Forms write only listed fields.** This is stricter than today and closes a gap (a
  hand-made POST can't set a writable field the form never showed), but every form must
  list its fields. The generated default form lists all writable fields, as now.
- **Generated pages**: keep generating pages for classes that have no page file
  indefinitely, or require every page to be declared once an app has moved over?
