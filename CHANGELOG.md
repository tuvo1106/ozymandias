# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **`conditional_formats` is refused outside `query_value` and `table`.** A
  timeseries, toplist or heatmap accepted them and drew nothing; the rule in
  docs/dashboards.md ("a field that belongs to another type is an error") now
  holds for them too. What that does to a definition that already has one:
  an ordinary stored dashboard still loads (a stored row is served without
  being re-validated), but saving it again needs the field removed — the
  editor lists it for removal. A **template**, though, is re-validated every
  time it is instantiated, so a stored template carrying one stops producing
  service dashboards (it is listed in `unreadable` and logged), and a
  provisioning file carrying one fails to provision. Remove the field from the
  file or the row.
- **The M1 query parameters are translated, not reimplemented.**
  `metric`/`filter`/`by`/`agg` are now written out as a metricql query and run
  through the one evaluator; `internal/query/simple` is gone. The response's
  `query` field shows the translation, which is how somebody migrating finds
  out what to type. Three behaviours moved with it. A bare `k` filter term
  ("has the tag `k` with no value") has no spelling in the query language, so
  it is widened to `k:*` and a warning says so. The 366-day cap on `to - from`
  is replaced by the evaluator's 30-second wall-clock budget, which bounds the
  same thing without guessing how much data a day holds. The bucket cap is
  1500, down from 10,000 — already more points than a chart draws. And one
  limit is new: a query node selects at most 1000 series, where M1 would
  happily aggregate a hundred thousand into one line. A bare query on a
  high-cardinality metric that used to draw a chart now asks for a narrower
  filter or a `by`.
  Every piece of a structured request is now validated against the parser's own
  rules before it is interpolated, because that path builds a program out of
  strings the caller sent: an `agg` of `x{*}} + sum:other{*`, or a tag value
  containing a brace, is refused rather than run.
- **The project is now `ozymandias`, and the repo is public** (ADR-0012). The
  working name it carried while the repo was private was a pun on a commercial
  product — not a name to publish under. Everything typed uses the short token
  `ozy`: binaries
  `ozyd` and `agent`, env prefixes `OZY_` and `OZY_AGENT_`, self-metrics `ozy.*`,
  wire headers `X-Ozy-*`, SDK packages `ozy`, config at `/etc/ozy`, data at
  `./data/ozyd`. **Breaking for every existing deployment:** re-create the config
  from `deploy/ozyd.yaml` and `deploy/agent.yaml`.
- **Block magic is now `OZCH`/`OZIX`.** Blocks written by earlier builds cannot
  be read; there is no migration, by choice.
- **The wire dialect is described as "extended StatsD"** rather than by the
  vendor name for the same grammar. The protocol is unchanged and the
  compatibility captures from `datadogpy` and `hot-shots` still pass.
- **The workflows' actions are current again, and stay that way.** Five had
  fallen far enough behind that GitHub was force-running them on a newer Node
  than they declared: checkout v4→v7, setup-go v5→v7, setup-node v4→v7,
  setup-uv v5→v10, golangci-lint-action v8→v9. A grouped monthly Dependabot
  config now watches them, so the next drift arrives as one pull request
  rather than as a warning nobody is reading. `ubuntu-latest` is kept on
  purpose through the Ubuntu 26 migration; the reasoning is in the workflow.
- **CI no longer lets the runner image choose a toolchain.** Go and Node were
  already pinned by `go.mod` and `web/.nvmrc`; uv, the Python interpreter and
  golangci-lint were not, so the SDK's 90% coverage gate could move to a
  different interpreter with no commit to this repo and surface as a red
  required check on an unrelated pull request. They are now
  `sdk/python/.tool-versions`, `sdk/python/.python-version` and
  `.golangci-lint-version`. `make lint` and `make sdk-check` warn — they do not
  fail — when the local binary differs from the one CI uses, so the pre-push
  gate and CI can no longer disagree silently.
- **GitHub Actions runs `make ci` on every pull request** (ADR-0013, supersedes
  ADR-0010) now that public-repo minutes are free — including `make sdk-check`,
  which the metered workflow used to skip, so both SDKs' 90% gates are enforced
  remotely too. The git hooks remain the fast gate.
- **The SDKs stay vendored** (ADR-0014, supersedes ADR-0007) — now because they
  are pre-1.0, not because the repo is private. Git install is documented for
  Python; a first publish would use PyPI `ozy` and npm `@tuvo1106/ozy`.

### Added

- **The Metric Summary page** (`/metrics/summary`): every metric by series
  count, highest first, and for one metric the tag keys that make its series,
  most values first. Backed by `GET /api/v1/metrics/cardinality` and
  `GET /api/v1/tags/cardinality`, and by two new `MetricStore` methods,
  `SeriesCounts` and `TagCardinality`, which count distinct series from the
  index — once however many places the TSDB keeps a series in. See
  ADR-0023, docs/api.md and docs/ui.md §2.
- **The Metrics Explorer takes a query.** A metricql box with the dashboard
  editor's completion and inline parse errors replaces M1's pickers; Run (or
  Ctrl/⌘+Enter) charts it, and the URL holds the query that was run
  (`?q=`). A legend table gives each line's last, avg, min and max. M1 links
  (`?metric=&filter=&by=&agg=`) still open: ozyd translates them, and the
  query it ran replaces them in the URL.
  See docs/ui.md §2, ADR-0022.
- **Save to dashboard** from the Metrics Explorer: opens a stored or new
  dashboard in the editor with a timeseries widget for the charted query
  added and unsaved (`?add=` on `/dashboards/{id}/edit` and
  `/dashboards/new`).
- **The dashboard editor** (`/dashboards/{id}/edit`, `/dashboards/new`): a
  12-column grid with drag and keyboard move/resize, a widget panel offering
  exactly the fields each type uses, a live preview drawn by the real widgets,
  JSON import/export, and *Save a copy* for provisioned dashboards and service
  template instances. Fields a type does not use, keys this build does not
  read and values it does not know are shown, never silently dropped or
  replaced; save failures are reported by kind. See docs/ui.md §4.
- **A query editor with completion and inline parse errors**: aggregators,
  functions and modifiers from the parser's own vocabulary (generated from
  `internal/query/metricql`, ADR-0021), metrics and tag keys/values from the
  store, `$variables` from the dashboard; the character
  `POST /api/v1/query/validate` names is underlined.
- A warning every widget on a dashboard carries is said once above the grid.
- A `table` colours its cells by its `conditional_formats`.

- **Dashboards are on screen.** `/dashboards` lists what is stored and every
  service a template covers; `/dashboards/{id}` and
  `/dashboards/service/{name}` draw one on the twelve-column grid the definition
  describes, with all six widget types — `timeseries`, `query_value`,
  `toplist`, `table`, `note` and `heatmap`. One selector per template variable,
  one time picker and one auto-refresh toggle for the whole page, all of it in
  the URL so a link shows the other person what you were looking at. Charts
  share a crosshair, the heatmap included. A page is **one** batch request for
  its line charts plus one per heatmap, and a refused query draws inside its own
  widget — beside whatever else that widget had to show — rather than blanking
  the page (ADR-0017). A widget type this build does not know says so in its own
  frame instead of taking the application down, which is what an older UI and a
  newer `ozyd` look like; so does a `reducer` it does not know, rather than
  dropping the row and leaving a short ranking that reads as a complete one. A
  `note`'s markdown is rendered as text, not HTML: anybody who can POST a
  dashboard can write one. Read the pages in `docs/ui.md`.
- **The heatmap widget draws the sketch itself**, not a percentile taken from
  it: a column per bucket, a band per bin, brightness logarithmic in how many
  observations fell there, and a real gap where nothing was recorded. Set
  `"yaxis": {"scale": "log"}` on one — a sketch's bins are geometric, so on a
  linear axis a latency distribution from 4 ms to 2 s lands in the bottom
  fraction of a percent of the chart. A log axis has no position for the zero
  bin or for a negative observation, so those are counted out and named in
  words instead of quietly turning the axis linear. The widget also says how
  accurate the bands are (α = (γ-1)/(γ+1), worst bucket) and how many groups a
  `by` matched that it is not the place to draw.
- **A dashboard per service, without anybody writing JSON for it.** A definition
  with `"template": true` is served instantiated rather than as itself:
  `GET /api/v1/dashboards/service/{name}` binds its `service` variable to one
  service and answers every template, and `GET /api/v1/dashboards/services` lists
  the names to offer. `deploy/dashboards/service.json` is the shipped template —
  throughput, 5xx rate, p95 latency and a latency heatmap from the
  `http.request.*` metrics every SDK sends. Its 5xx rate is a chart rather than
  a number, because an empty selection produces no series rather than a series
  of zeros: as a number a healthy service renders an empty square, which reads
  as "broken". `docs/dashboards.md` says so where somebody writing a template
  will read it.
  Instantiation **binds** the variable rather than rewriting `$service` in the
  query text, so the definition a reader sees and the query the server runs stay
  the same string. The services are the tag values of the metrics the templates
  themselves query — *not* "seen in the last day" as the plan asked for, which
  nothing in ozymandias can answer: the tag index takes no time range and the
  metadata database tracks metrics rather than services (ADR-0020, and the M3
  spec is amended). So a service that stopped reporting is listed until its
  series fall out of retention, and a service that reports none of a template's
  metrics is not listed at all — its instance would be a grid of empty charts.
  An unknown name is a `404` rather than that grid, because a typo in a runbook's
  URL reads as "the service is down". Discovery is capped at 500 *distinct*
  tag-index lookups and 1000 services per request, and says `"truncated": true`
  when it hits either — uncapped, one `GET` could ask the store about every
  metric of every template, and nothing bounds how many templates exist.
  Lookups are memoized across templates, which are expected to overlap, and a
  request for one service stops as soon as that service turns up.
- **The distribution behind a metric, not just a number from it.**
  `GET|POST /api/v1/query/sketch` answers a new `dist:` aggregator —
  `dist:http.request.latency{service:api} by {route}` — with the merged sketch
  per bucket, as bins, which is what a heatmap draws. Each bin is
  `[lower, upper, count]`: resolved value bounds rather than the sketch's bucket
  index, in value order, with zero in a bin of its own and negatives reversed so
  the axis is not mirrored. `count`, `sum`, `min` and `max` come along exactly,
  beside the approximate shape, and each bucket reports its own `gamma` — the
  error bar on its bins — because two groups can legitimately carry different
  relative accuracies and one number for the response would describe the first
  and be applied to the rest.
  A bucket nothing landed in is absent, so a quiet metric does not spend its
  response on 1500 ways of saying nothing happened; a response is refused past
  200000 bins, because a sketch's bin count grows with the ratio between its
  largest and smallest value and nothing about the query shows that.
  `dist:` is an aggregator because merging *is* how a distribution's series
  combine, but its answer is not a number: it has no value on `/api/v1/query`
  and cannot take part in arithmetic, and both refusals name the endpoint that
  does answer it (ADR-0019).
  A **heatmap widget's query must now be `dist:`**, and no other widget's may be:
  `docs/dashboards.md` previously showed a percentile there, which cannot be
  drawn — the endpoint refuses it, and `/api/v1/query` returns one number per
  bucket rather than bins. The validator catches both mismatches when the
  dashboard is saved rather than on every draw. No shipped dashboard used a
  heatmap, so nothing stored changes meaning.

- **A whole dashboard in one request.** `POST /api/v1/query/batch` takes a list
  of queries and one window, interval and set of template variables, and answers
  with one result per query — each with its own `status`, `interval`, `series`,
  `warnings` and, on a failure, a `code` and `error`. The interval is per result
  because `.rollup(method, seconds)` sets the grid of the query it is written on,
  so one batch can hold results on different bucket widths. One broken widget no longer blanks the
  other eleven: the HTTP status describes the request, and whether each *query*
  worked is that query's own business (ADR-0017). The batch shares one
  30-second deadline rather than one per query, because fifty queries of thirty
  seconds each is not a timeout.
  Queries that select the same series are **selected and bucketized once** for
  the whole batch: a query node splits into select-and-reduce, which is cached
  per request, and group-and-aggregate, which is not. Six dashboard-shaped
  queries over a thousand series take 4.9 ms instead of 10.8 ms and ask the
  store once instead of six times. The cache is capped at 16 MiB per request,
  after which the batch keeps answering without sharing (ADR-0018).

- **Dashboards are stored, validated and provisioned.** `GET/POST
  /api/v1/dashboards` and `GET/PUT/DELETE /api/v1/dashboards/{id}`, with the
  definition's JSON specified in [docs/dashboards.md](docs/dashboards.md):
  twelve-column layout, six widget types (`timeseries`, `query_value`,
  `toplist`, `table`, `heatmap`, `note`), template variables, y-axis bounds and
  conditional formats.
  A definition is **validated on the way in and stored verbatim** — byte for
  byte, formatting included — and served back from those bytes rather than from
  a re-encoding, so its key order, every number as written, and any field this
  build does not know about all survive an export from one ozyd and an import
  into another. Whitespace and `<`/`>`/`&` escaping do not: a response is
  compacted and escaped by Go's JSON encoder whatever the stored row looks like.
  The list response carries `count` (the number of rows it could encode, not the
  number in the database) and an always-present `unreadable` array naming any
  row whose stored definition could not be spliced. A definition containing
  `id`, `provisioned`, `created_at` or `updated_at` is refused rather than
  served, because those are the database's and a response cannot carry a key
  twice and still mean one thing to every parser.
  Validation refuses what is certain to fail later: a query that does not parse,
  an unknown widget type, a layout off the grid, a field belonging to another
  type, and — the one that cannot be checked one widget at a time — a `$var` no
  `template_vars` entry declares. It deliberately does *not* run the queries: a
  dashboard for a service that has not shipped yet is a legitimate dashboard,
  and saving one should not depend on the data being there.
- **Dashboards-as-code.** Directories in `provisioning.paths` are read at
  startup and upserted by the definition's `uid`, so an app repo keeps its own
  dashboards in git and mounts the directory. Such a dashboard is read-only over
  HTTP (`409`, saying to edit the file) because provisioning runs again at every
  restart and would otherwise silently undo the edit.
  **One bad file does not stop the others, or startup** — the directories come
  from config and an app mounts its own, so one team's typo must not be an
  outage for everybody's monitoring at the moment monitoring is most wanted.
  Failures are logged with the path and counted. An unchanged file is not a
  write, so `updated_at` keeps meaning what it says across restarts.
- **A first `Home` dashboard** (`deploy/dashboards/home.json`), baked into the
  image: ingest rate, refusals, store and head size, per-agent throughput.
  Per-service health cards arrive in M6 when there are monitors to colour them
  by.
- **A template variable's name is now case-insensitive end to end.** The lexer
  lower-cases a `$name` as it reads one, exactly as it does a tag key, and the
  query API lower-cases both the `var.<name>` parameters and a JSON `vars`
  object's keys. Previously the lexer kept the case while the dashboard
  validator folded it, so a dashboard declaring `env` and querying `$Env`
  validated and *then* failed to render on every widget with "$Env is not
  bound" — and nothing between the two was positioned to notice.
- **`metricql.Walk` and `metricql.Variables`** are exported, so more than one
  caller can ask a question of a whole expression. The evaluator's private
  traversal is gone in favour of the shared one; `Variables` is what lets a
  dashboard's `$vars` be checked against its declarations.

- **`/api/v1/query` speaks the query language.** `?q=` takes any metricql
  expression, so a rate, a ratio of two queries, a `top(…)` or a percentile is
  now one request where M1 could only select-group-aggregate one metric.
  `POST /api/v1/query` takes the same request as JSON, because a generated
  dashboard query outgrows a URL. `var.<name>=` (repeated, for a multi-select)
  binds the query's template variables.
  Responses carry three new fields: `query`, the canonical spelling of what was
  actually evaluated; `scope` on each series, so every client names a line the
  same way instead of each inventing its own join of the tags; and `warnings`,
  always present, for what the caller should know but that did not stop the
  query.
- **`POST /api/v1/query/validate`** answers "does this parse, and if not,
  where" with a 1-based column, and `200` either way — it is for an editor
  calling on each keystroke, which wants something to underline rather than an
  exception.
- **A `499` when the caller hangs up.** An editor that re-queries on every
  keystroke abandons requests constantly; each one used to be a `500` and an
  `ERROR query failed` line, which is how a log stops being worth reading.
  Only the *request's* context counts — work that cancelled itself while the
  caller was still waiting has failed, and is still a `500`.
- **A `503`, not a `400`, when a metric's sketches disagree about accuracy.**
  Two writers using different relative accuracies is a property of what is
  stored, and no rewrite of the query fixes it, so telling the caller their
  query was bad sent them looking in the one place the problem was not. The
  message names the metric, because that is what an operator goes looking
  with.

- **The metricql evaluator** (`internal/query/metricql/eval`): an AST now runs
  against the stores. Select, time-aggregate, group, space-aggregate, then fill,
  functions and arithmetic — in that order, because a fleet-wide rate is the sum
  of each host's rate and a fleet-wide p95 is the quantile of a merge, and only
  this order gives both. Every aggregator, every rollup method, every fill mode,
  `.as_rate()`/`.as_count()` with the type rules that make them mean something,
  `IN` lists, template variables, and all ten functions including `top`,
  `timeshift` and `histogram_quantile`. Percentiles are answered by merging
  sketches per output bucket, as in M2.
  Every node of one request is evaluated onto one bucket grid, so two
  `.rollup()`s asking for different widths is an error rather than a resample
  ([ADR-0016](docs/adr/0016-one-grid-per-query.md)) — resampling would have to
  either divide or repeat, and both invent data that then looks measured.
  A template variable bound to several values of one key is a *choice*, like
  the `IN` list it stands in for — a dashboard's multi-select means either
  host, not both at once.
  Not yet wired to the HTTP API: `/api/v1/query` still serves M1's structured
  query until the endpoint moves over.

- **metricql, the query language** (`internal/query/metricql`, specified in
  [docs/query-language.md](docs/query-language.md)): a lexer, a
  recursive-descent parser, and a printer that turns an AST back into one
  canonical spelling. This is the parsing half of M3 §1; the evaluator that
  runs an AST lands next, and until then M1's structured query is still what
  `/api/v1/query` serves.
  The language is not lexically uniform — `route:/api/items` and `a / b` use
  the same byte for different jobs, and a tag key may contain `-` and `/` —
  so the lexer is pull-based and takes its mode from the parser rather than
  running as a separate pass. AST nodes carry no source positions, which
  makes `parse(print(ast)) == ast` an exact property rather than a comparison
  modulo fields nobody reads; it is checked by `pgregory.net/rapid` and by
  `FuzzParse`. A tag value deliberately cannot contain a brace — in a plain
  `k:v` or inside an `IN` list — so that a missing `}` is an error at the right
  column instead of a query that parses wrongly and silently.

- **Percentiles are real: `p50`, `p75`, `p90`, `p95` and `p99`** on any
  `distribution` metric, and statsd type `d` now means one. Until now `d` was
  a synonym for `h`: the agent computed a p95 locally from a reservoir of at
  most 10,000 samples per bucket and shipped it as a gauge. That number
  describes one host and cannot be combined with another's, so a fleet-wide
  p95 was never available — only the mean of several hosts' p95s, which is a
  different number with no error bound at all. The agent now builds a DDSketch
  per context per bucket and ships it whole on `POST /v1/sketches`
  (wire-protocol §D); a query merges every sketch of every selected series in
  each output bucket and takes the quantile of the result. Merge first,
  quantile second. **Breaking for anyone reading `<metric>.avg`, `.median` or
  `.95percentile` off a `d` metric:** those series are no longer produced —
  `<metric>.count`, `.sum`, `.min` and `.max` are, and they are exact rather
  than estimated. Type `h` is unchanged.
- **`internal/sketchstore`**, a Pebble-backed store for those sketches under
  `data_dir/sketches/`, independent of `storage.metric_store` so percentiles
  work on either engine. Keyed by a hash of the series rather than by an
  assigned id ([ADR-0015](docs/adr/0015-sketch-storage-and-identity.md)):
  the TSDB's ids are not stable identity, since the head forgets a series when
  it truncates and gives it a new one when it reappears. Retention uses the
  existing `storage.retention` window, sweeps hourly, and runs one
  `storage.block_range` behind the TSDB's — which expires whole blocks, so a
  `.count` outlives the cutoff and a sketch must outlive it too, or `p95`
  goes null under a line the count chart still draws. The value layout is
  specified byte for byte in `docs/formats/sketch.md` and pinned by a golden
  file.

- **DDSketch (`internal/sketch`)** — the quantile sketch M2 part two is built
  on. Percentiles do not average, so a p95 cannot be computed from per-host
  p95s; a sketch can be merged, which is what makes `p95 by {route}` across a
  fleet answerable at all. Buckets are geometric (`k = ceil(log_gamma v)`), so
  the error is *relative*: every estimate is within 1% of the true value
  whether it lands at 3ms or 30s, on a store that is 2048 buckets wide
  regardless of how many observations it has seen. Merging is bucket-wise
  addition and therefore exact and order-independent; count, sum, min and max
  are carried alongside and are not approximations. Values beyond what the
  store can index collapse into the lowest bucket — the end nobody queries —
  and the package is not concurrency-safe, by design: one owner per sketch.
  The decoder entry points (`AddBin`, `SetAggregates`) validate and return an
  error rather than absorbing what they are handed: they will be fed bytes
  from another process, where a NaN count or an out-of-range bucket index is
  the difference between a rejected payload and a sketch that answers every
  percentile confidently and wrongly.

- **Boundary tests for the chunk encoder's delta-of-delta buckets**, found by a
  one-off mutation-testing run over `internal/tsdb/chunkenc`. A delta-of-delta
  filed one bucket too wide still decodes to the right number — it only spends
  more bits — so no round-trip test could see it. The new tests assert the
  *cost in bits* of every bucket edge, which is what the bucket is, plus the
  duplicate-timestamp case on the delta-of-delta path, which the existing table
  covered for the first two samples but not the third.
- **A boundary test for the chunk encoder's leading-zero clamp.** The count is
  stored in five bits, so 32 or more is clamped to 31; at exactly 32 an
  unclamped write keeps only the low five bits and stores it as 0, and the
  decoder then rebuilds the value against a window 32 bits too wide. It turns
  a value just above 1.0 into exactly -1.0. It needs the XOR of two consecutive
  values to land in [2^31, 2^32), which neither the property test nor the
  fuzzer had hit.

### Fixed

- **A rejected sample no longer leaves an empty series behind.** The head
  created and indexed a new series before checking whether its sample was too
  old to accept, so a backfill of fresh ids behind a block cut left empty
  series until the next truncation: listed by autocomplete, counted by the
  Metric Summary, and counted against the per-metric series limit, which could
  then refuse real series. Out-of-bounds samples are now rejected before any
  series is created, and are no longer written to the WAL.
- **One unusable dashboard row no longer sinks the whole list.** `GET
  /api/v1/dashboards` encoded every row in one call, so a single definition that
  could not be spliced made the endpoint a `500` — and the list is what a
  dashboard picker is built on, so the blast radius of one hand-edited row was
  "nobody can open anything". Rows are encoded one at a time now; a row that
  cannot be rendered is named in a new always-present `unreadable` array and the
  reason is logged with its id, rather than vanishing or taking the others with
  it.

- **A dashboard's stored definition could have overridden its own metadata.**
  The response splices the definition's fields beside the database's `id`,
  `provisioned` and timestamps, and wrote the metadata *first* — with a comment
  claiming that this stopped a definition containing an `id` from overwriting the
  database's. It is the opposite: in JSON the last of two duplicate keys wins, so
  metadata-first meant the definition won. The metadata is written last now, and
  a definition that claims one of those four keys is refused outright — ordering
  alone only works for parsers that keep the last duplicate, which is what the
  ones we have do and not something JSON promises (`encoding/json/v2` rejects
  duplicate object names), and a response whose meaning depends on the reader's
  parser is not an answer. Nothing could reach it — those are not fields of a
  definition and unknown fields are refused on both write paths — so this was an
  imaginary defence rather than a live bug, which is its own kind of problem.
  The row's id and the reason now reach an operator once per row rather than
  once per request: `GET /api/v1/dashboards` is what a dashboard picker polls,
  and an `Error` record per poll for a row that stays broken until somebody
  edits the database buries the rest of the log.

- **A JSON response could be an empty `200`.** `writeJSON` encoded straight to
  the `ResponseWriter`, so the status line was already sent when the encoder
  failed — and the error was discarded. A success the client cannot parse and
  the server never mentions. It now marshals into a buffer first, which makes
  that a `500` with a body, in JSON like every other response. Every endpoint in
  `internal/api` had the failure mode; a provisioned dashboard is what surfaced
  it, because a definition read from a file ends in a newline and the response
  builder assumed its last byte was `}`.
- **A duplicate dashboard `uid` was a `500`.** It is now a `409` naming the uid,
  classified by SQLite's constraint *code* rather than by matching on an error
  message. It is reachable on the import path the docs advertise — export from
  one ozyd, import into another — so it had to be the caller's error.
- **Provisioning could silently take over an API-created dashboard** that
  happened to share a `uid`: it overwrote the definition, flipped `provisioned`
  to 1, and left the API answering `409` to every attempt to restore it. It now
  refuses that file and says which dashboard is in the way — the same reasoning
  that makes a provisioned dashboard read-only, pointed the other way.

- **Eight storage defects from a whole-tree review** (issues #4–#11), all in the
  M2 engine:
  - `block`: a chunk record's length prefix was validated with `used+length+4
    == recLen`, which overflows — a ten-byte uvarint can wrap the sum back onto
    `recLen`, and the slice taken next wrapped too, panicking on data that came
    from `index.dat`. An offset near the top of the range wrapped the same way.
    `chunks.dat` was also the only on-disk decoder in the TSDB with no fuzz
    target, which is why this survived three review rounds; it has one now.
  - `head`: a series record's tag count was bounded by the record length but a
    tag costs two bytes and `tsdb.Tag` is 32, so a record could make the
    decoder reserve 32× its own size — half a gigabyte at the 16 MiB WAL
    ceiling — before parsing a byte. The head's record decoders gained a fuzz
    target too.
  - `head`: an `Append` batch above ~600k samples was encoded as a single WAL
    record and rejected for exceeding `MaxRecordSize`, so a dense intake
    request could never succeed and every retry failed identically. Batches are
    now split across records in one `wal.Log` call, which keeps log order.
  - `db`: `CutBlock` could freeze the head *above* wall-clock time. The cut
    threshold bounds `cutAt` by the head's MaxT, and `wire.MaxFutureSkew`
    accepts samples ten minutes ahead, so with a short `block_range` one
    fast-clocked client made the store reject every real-time sample with
    `ErrOutOfBounds` until the clock caught up.
  - `db`: a failed `block.Open` after a successful `block.Write` left the block
    on disk untracked, and the next tick wrote a second copy of the same
    samples. The cut is now all-or-nothing.
  - `block`: `Delete` fsynced the tombstone but not its directory, so a crash
    mid-unlink could leave a block with missing files and no tombstone — which
    is the one state that stops ozyd starting.
  - `compact`: the run scan stopped at the first block of another level or
    resolution. Once rollups land they interleave with their sources, every run
    would end at length one, and compaction would stop permanently for both
    resolutions. It now skips rather than stops.
  - `config`: `storage.retention: 0` read as "off" (as `max_bytes: 0` does) but
    reached the store as unset and came back as the 15-day default, quietly
    deleting data. It is now rejected, with the negative form named in the
    error.
- **A query landing on a block cut could return a hole** (issue #3). `DB.Select`
  snapshotted the block list, read every block, and only then read the head. A
  cut inside that window publishes its block *after* the snapshot and truncates
  the head *before* the head read, so the block range it moved was in neither
  half of the answer — a successful query, missing a contiguous range, with
  nothing logged. The head is now read first, which is the order the cut's own
  publish-then-truncate sequence was written to be safe against; the samples are
  briefly in both places instead of neither, and the merge deduplicates them.
  Not a durability bug: nothing was ever missing from disk.
- **`FakeClock.Advance` no longer costs one iteration per ticker period.** A
  ticker left behind by a big `Advance` delivers its first missed deadline and
  drops the rest — which the clock already did, one period at a time, re-sorting
  its waiters on each. `TestDB_RetentionCanBeDisabled` advances 10,000 hours
  against a 6s maintenance ticker and spent 249s of the `internal/tsdb/db`
  package's 299s doing it. The package now runs in 42s, with no test removed and
  no assertion changed.
- `make test` now passes `-timeout 25m`. `internal/tsdb/db` costs ~5 minutes
  under `-race` locally and ran 600.06s on a CI runner, which Go's 10-minute
  default killed mid-test.
- **`GET /api/v1/query` no longer panics on an out-of-range `from`/`to`.** The
  bucket count is computed from the raw parameters, and
  `?from=-9223372036854775808&to=1790000000&interval=10` wrapped it negative,
  slipping past the 10,000-bucket cap and reaching a `make([]float64, n)` that
  panicked outright. `from` and `to` are now required to be unix seconds in
  `[0, 253402300799]`, which is what the bucket arithmetic actually assumes.
- **A query's range is capped at 366 days.** The bucket cap bounds the answer,
  not the work: `?from=0&to=now&interval=200000` is a legal 8,951 buckets and
  asked the store to read 55 years of samples.
- **One StatsD line can no longer poison an agent's whole flush.** A sample
  rate is used as its reciprocal, so `x:1|c|@1e-320` passed the `(0,1]` check
  and scaled a single increment to `+Inf` — which never leaves the bucket, and
  which `wire.Point` then refuses to marshal. The parser rejects a rate that
  small, and the aggregator drops any sample whose scaled value is not finite.
- **The forwarder skips a series it cannot encode instead of dropping the
  batch.** One unencodable series used to fail the whole payload, discarding
  every other series in that flush — including the agent's own self-metrics —
  once per interval for as long as the bad input kept arriving.
- **`scripts/check-docs.sh` checks self-metrics again.** Its pattern still
  looked for `ozymandias.*` names, so after the rename it matched none of the
  33 registered metrics and passed whether or not they were documented — in
  the pre-commit hook and in `make ci` alike. It now matches `ozy.*` and fails
  if it ever matches nothing at all.
- **`make sdk-release` no longer aborts on the Python wheel.** The filename
  was hard-coded as `ozymandias-*.whl` while the package is `ozy`, so the
  script announced a file `uv build` had never produced and would have failed
  at the copy the first time app-python was a target. The name now comes from
  `pyproject.toml`, and a mismatch fails at the build step.
- **A re-run of an interrupted WAL truncation no longer loses records.** A
  `Truncate(n)` that published `checkpoint.n` and then died partway through
  deleting the segments it was built from left the next pass — which computes
  the same `n` — rebuilding the checkpoint from the segments that survived and
  renaming over the one that held the rest. Anything that lived only in the
  already-deleted segments was in no block and no log. The rebuild now reads
  the checkpoint at `n` as one of its sources.
- **A chunk that fails to decode fails the read instead of shortening it.**
  `head.Select` stopped at the bad chunk and returned the samples it had,
  which a caller cannot tell from a series that is genuinely that short — and
  its caller is the block cut, which writes a block from that snapshot and
  then truncates the head and the log to match. One decode error dropped that
  chunk and every later chunk of the series from all three. `Head.Select` now
  returns an error, as `block.Block` already did, and a cut that cannot read
  the head is abandoned with everything still in place.
- **The canonical number form on the wire is now one form, not three.**
  wire-protocol.md §A said "the shortest string that round-trips a float64",
  which every implementation satisfied while writing different bytes: Go's
  `strconv` `'g'` turns a byte count of `1048576` into `1.048576e+06`,
  JavaScript's `String` stays positional to `1e21`, and Python's `repr`
  switches at `1e16`. §A now specifies the window (positional in `[1e-4,
  1e16)`, exponent form outside it, exponent padded to two digits), the Go
  writer and the Node SDK implement it, and seven golden cases pin it. Every
  form parsed back to the same float64, so nothing was ever corrupted — but
  the SDKs' promise of identical bytes was not true, and the goldens could
  not catch it because no case fell in the divergent windows.
- **The Node SDK drops a non-numeric value instead of coercing it.** From the
  CJS build, `gauge("queue.depth", null)` recorded a real `0` — as did `[]`,
  while `"5"` recorded `5` — where the Python SDK refuses all three.
- **CI runs the fuzz targets.** The workflow claimed parity with `make ci`
  minus smoke, the crash loop and `fuzz-long`, but `make ci` also runs
  `make fuzz FUZZTIME=10s` and no step did — leaving the guard on the
  corrupt-input decoders enforced only by the local pre-push hook.
- **CI runs on every pull request**, with no `paths-ignore`. `ci` is a required
  status check on `main` now, and a required check that is never *reported* is
  not a check that passed: a skipped workflow leaves the pull request waiting
  on a status that will never arrive, so a docs-only change could never be
  merged. Running it is not a no-op either — `make ci` includes
  `make docs-check`.
- **Compaction deletes its source blocks only once the merged block is
  serving.** `compact.Run` unlinked them before the database had opened the
  merged block or swapped it in, so a failure in either step left blocks gone
  from disk and still in `db.Blocks()` — and the next tick planned the
  identical run and wrote another full copy of the merged block, every tick,
  all of it charged against `MaxBytes`. Deleting is now `compact.DeleteSources`,
  called after the swap, and a source that will not delete is logged and left
  for `dropSuperseded` rather than failing a compaction that has happened.
- **Damage in the last WAL segment is only a torn tail if it reaches the end of
  the file.** Being in the last segment was the whole test, so a bad checksum
  at offset 0 of a 32 MiB segment discarded every record after it with no
  error, no log line and no counter — and `Repair`'s `TruncateTail` then made
  it permanent. A crash can only tear the record being appended, so damage
  with whole records written after it is corruption and is now reported as
  `ErrCorrupt`. Where the damaged record's extent is known (a checksum
  mismatch means every promised byte was there), the bound is exact.
- **Two blocks covering the same time range no longer hide each other.** The
  cross-source merge in `Select` deduplicated only against the last sample it
  had appended, which is correct only if the sources are prefix-ordered in
  time. They are not after a crash between writing a merged block and deleting
  its sources, and will not be once rollup blocks land: every sample the second
  source held for an instant the first had nothing for was dropped. Samples are
  now merged, sorted and deduplicated by timestamp when any source arrives out
  of order, with the oldest source still winning an instant they both claim.
- **Group-commit has its own goroutine.** It shared one select with the
  maintenance pass, so no WAL fsync happened for the whole of a block cut, a
  compaction rewriting three blocks and a retention sweep. With
  `wal_sync_on_append: false` the window an acknowledged sample spends in the
  page cache was therefore not `wal_sync_interval` — as `Options.SyncInterval`
  and `deploy/ozyd.yaml` both say — but the length of the longest pass.

### Added

- `SECURITY.md` and `CODE_OF_CONDUCT.md`.

- **Samples are append-only** (ADR-0011). A sample at or before a series' newest timestamp is
  rejected and reported in `AppendResult.Rejected` instead of overwriting it; an exact repeat
  of the newest sample stays a no-op, so at-least-once agent retries are still safe. The naive
  SQLite store previously did last-write-wins and now matches, because the M2 TSDB stores
  samples in append-only Gorilla chunks and physically cannot overwrite one.

### Added

- **The real TSDB (M2).** Gorilla-compressed chunks, a segmented write-ahead log, an
  inverted index, an in-memory head, immutable on-disk blocks and leveled compaction, tied
  together by `internal/tsdb/db` behind the existing `MetricStore` interface. It is now the
  default metric store; `storage.metric_store: naive` selects the M1 SQLite store, which is
  kept as the differential-test oracle and as an escape hatch.
- `storage.*` configuration: block range, retention, disk cap, cardinality limit and WAL
  sync policy, each with a `OZY_STORAGE_*` override (see `docs/operations.md`).
- `ozy.tsdb.*` self-metrics: rejected samples, head size, block count and disk usage.
- `ozy.tsdb.wal_syncs`: write-ahead log flushes since startup. Monotonic, so its
  *rate* is the signal — a flat stretch means acknowledged samples are staying in
  the page cache longer than `wal_sync_interval`.
- A 50-iteration crash loop (real `SIGKILL`s of a child process), a 72-hour fake-clock
  lifecycle test, a concurrency stress test, format goldens and fuzz targets for the chunk
  and WAL decoders. Between those and three rounds of code review they found nine ways an
  acknowledged sample could be lost, two ways disk could leak, one way a metadata query
  could kill the process and one way shutdown could panic; all are fixed, and none ever
  shipped. See `docs/notes/M2.md`.
- `wal.Repair`, run before the log is opened for writing: a torn tail is now removed rather
  than left for the next append to queue up behind.

- **Metrics, end to end (M1).** An extended StatsD counter sent from an app is aggregated by the
  agent, forwarded to `ozyd`, stored, and queried back through the UI.
- `pkg/wire`: the shared name/tag rules and the `/v1/series` payload, with goldens both SDKs
  and the Go code load, so the three implementations cannot drift.
- Agent statsd server: a zero-allocation extended StatsD parser (45 ns/line), multiple UDP readers
  feeding workers through a bounded queue that drops and counts rather than growing.
- Agent aggregator: sharded contexts, 10s buckets, sample-rate scaling, counter zero-fill with
  expiry, gauge last-write-wins, histogram reservoir with `.avg`/`.min`/`.max`/`.median`/
  `.95percentile`/`.count`, and late samples folded into the oldest open bucket.
- Agent forwarder: payloads split at 5000 series / 2 MiB, gzipped, retried with full-jitter
  backoff honouring `Retry-After`, buffered in a 16 MiB drop-oldest queue, with a final
  delivery attempt on SIGTERM.
- `ozyd` intake `POST /v1/series` with per-series rejection, a metadata DB that pins a
  metric's first-seen type, and the `MetricStore` interface with a naive SQLite implementation.
- Query API `GET /api/v1/query` plus `/metrics`, `/tags` and `/tags/values`, with bucketing,
  grouping and cross-series aggregation; empty buckets serialize as `null`.
- Metrics Explorer UI at `/metrics/explorer`: autocomplete, filter chips, group-by, aggregator,
  time range, auto-refresh and a uPlot chart, with the query state in the URL.
- Zero-dependency SDKs: `sdk/python` (Python ≥ 3.12) and `sdk/node` (Node ≥ 22, ESM + CJS),
  identical in shape, each wrapped so they cannot throw into the host app.
- Both binaries report their own `ozy.*` metrics through the pipeline, tagged `host:`.
- `cmd/loadgen` (`statsd-flood`) and `examples/cron-script.sh` — a metric from a shell script
  with `nc`, no SDK.
- extended StatsD compatibility goldens captured from datadogpy and hot-shots, with
  `scripts/capture-statsd-compat.sh` to refresh them.
- Docs: DESIGN.md §9 metric write path (with diagram) and §10 aggregation model,
  `docs/benchmarks.md`, the agent config reference and a metric-not-arriving checklist in
  `docs/operations.md`, and M1 learning notes.

- Go module `github.com/tuvo1106/ozymandias`, Makefile entry points, golangci-lint v2 config,
  pre-commit hooks for Go formatting and lint, and CI with the coverage, docs-drift,
  no-app-coupling and fuzz gates from `docs/plan/testing.md` and `documentation.md`.
- `ozyd` and `agent` binaries: `/healthz`, `/debug/vars` self-metrics, graceful shutdown
  on SIGTERM, and a `healthcheck` subcommand for distroless images.
- Layered config (defaults < file < `conf.d` fragments < env) with strict unknown-key
  checking; commented reference files `deploy/ozyd.yaml` and `deploy/agent.yaml`
  (ADR-0009).
- Test support: fake clock, `Eventually`, goroutine-leak checker.
- Web UI shell (Vite, React, TypeScript, Tailwind) embedded in `ozyd`: sidebar with every
  planned section and its milestone, server status on the home page, SPA routing with
  immutable asset caching.
- Docker image (distroless, non-root, ~28 MB, UI embedded) and compose stack; `make up` waits
  until both services are healthy.
- `make smoke`: 14 end-to-end checks against the running stack, including clean exit on SIGTERM.
- `make dev`: both binaries natively plus the Vite dev server, stopped together.
- Docs: DESIGN.md (as built), docs/operations.md, ADRs 0002–0008 and 0010, M0 learning notes.

### Changed

- The git hooks are the CI (ADR-0010). pre-commit runs lint, race tests with coverage gates,
  docs and coupling checks, and the web typecheck, lint and related tests for whatever is
  staged; pre-push runs the full `make ci`. The GitHub Actions workflow is manual-only.

### Fixed

- Config: a YAML file holding only comments loads as empty instead of failing with `EOF`.
- Config: a null value (e.g. `tags:` with every item commented out) merges as absent, so a
  later fragment's list still appends.
- `testutil.FakeClock`: `Stop` and `Reset` discard an unread fire, matching Go 1.23+ timers.
