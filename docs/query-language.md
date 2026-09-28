# metricql

The query language the dashboards and `/api/v1/query` are written in. This
document is normative: the grammar below is what `internal/query/metricql`
implements, and a change to one is a change to the other in the same commit.

A query reads left to right as the pipeline that answers it:

```
sum:http.request.count{service:api,!status:2*} by {route}.as_rate()
└┬┘ └────────┬───────┘ └──────────┬─────────┘    └──┬─┘  └───┬───┘
 │           │                    │                 │        └ 5. modify
 │           │                    │                 └ 3. group
 │           │                    └ 1. select
 │           └ of this metric, time-aggregated (2)
 └ 4. combine each group with this
```

## Grammar

```ebnf
expr      = term { ("+" | "-") term } ;
term      = factor { ("*" | "/") factor } ;
factor    = "-" factor
          | number | query | func_call | "(" expr ")" ;

func_call = ident "(" [ arg { "," arg } ] ")" ;
arg       = expr | number | string ;

query     = space_agg ":" metric "{" filter "}"
            [ "by" "{" key { "," key } "}" ] { "." modifier } ;
space_agg = "avg" | "sum" | "min" | "max" | "count"
          | "p50" | "p75" | "p90" | "p95" | "p99" ;

filter    = "*" | matcher { "," matcher } ;
matcher   = [ "!" ] key ":" value
          | [ "!" ] key " IN " "(" value { "," value } ")"
          | "$" ident ;

modifier  = "rollup" "(" method [ "," seconds ] ")"
          | "as_rate" "(" ")"
          | "as_count" "(" ")"
          | "fill" "(" mode [ "," seconds ] ")" ;
method    = "avg" | "sum" | "min" | "max" | "count" | "last" ;
mode      = "null" | "zero" | "last" | "linear" ;

metric    = letter { letter | digit | "_" | "." } ;
key       = letter { letter | digit | "_" | "." | "-" | "/" } ;
value     = ? any text up to the next "," "{" "}" or, in a list, ")" ? ;
ident     = letter { letter | digit | "_" | "." } ;
number    = digit { digit } [ "." digit { digit } ] [ ("e"|"E") ["+"|"-"] digit { digit } ] ;
string    = '"' { character | '\"' | '\\' } '"' ;
seconds   = ? a whole number from 1 to 2678400 ? ;
```

Case: `by` and `IN` are keywords and are accepted in any case. An aggregator
is accepted in any case. **A tag key is lower-cased as it is read**, because
the intake lower-cases every key it stores, so a filter typed in capitals
would otherwise match nothing and say nothing about why. **A template
variable's name is lower-cased too**, so `$Env` and `$env` are one variable and
whatever binds either binds both — a name that did not fold would let a query
parse against a declaration it can never be matched to. A metric name and a
tag value are taken exactly as written — the store distinguishes them.

Whitespace between tokens is insignificant, and a query may span lines. A value
is trimmed of it on both sides — the same whitespace, newlines and carriage
returns included — so `k: a ` selects `a` and a filter written across two lines
means what it looks like.

### What a value may not contain

A tag value is free text: `route:/api/items`, `url:http://host:8080/p`,
`version:1.2-rc*`. It is delimited by what follows it and by nothing else,
which is why the lexer reads it in its own mode (see the package comment on
`internal/query/metricql`).

It therefore cannot contain `,`, `{`, `}`, or `)` inside an `IN` list. The
comma is the wire format's own tag separator, so no stored tag has one either.
The braces are **given up on purpose**: without that rule, forgetting a
closing brace — `{service:api by {route}` — makes ` by {route` part of the
value and the query parses, wrongly and silently. Stopping at a brace turns
the commonest typo in the language into an error at the right column. A tag
whose value contains a brace exists in the store and cannot be selected by
name; `key:*` still finds it.

## Stages

### 1. Select

`metric{filter}` chooses series. Matchers are ANDed. Within one `IN` list the
values are ORed.

| Written | Means |
|---|---|
| `{*}` | every series of the metric |
| `{service:api}` | the tag `service:api` is present |
| `{service:ap*}` | some `service` value matches the glob (`*` only) |
| `{!service:api}` | the series has no `service:api` tag — **including series with no `service` tag at all** |
| `{status IN (500,502)}` | `status` is one of these |
| `{$env}` | whatever the dashboard's `env` template variable expands to |

Negation matching series that lack the key entirely is the same rule
`tsdb.Matcher` uses, so a filter means one thing at every layer.

### 2. Time-aggregate

Each selected series is reduced into buckets of `interval` seconds. The default
method comes from the metric's type — a gauge averages, a count sums, a
distribution merges its sketches — and `.rollup(method[, secs])` overrides it.
An empty bucket is null, not zero.

**Buckets are aligned to the epoch, not to `from`.** The first bucket starts at
the largest multiple of `interval` at or before `from`, so the same query asked
one second later returns the same buckets with the same timestamps instead of a
chart that redraws as "now" moves. The cost is that the first bucket can begin
before `from` and include samples from before it; a chart's leftmost point is
therefore the one not to read too closely.

This order is the reason a dashboard reads correctly: ten 10-second counts
become one 100-second count *before* anything is summed across hosts.

### 3. Group

`by {k1,k2}` puts series with equal values for those keys in one group. A
series missing one of the keys is grouped under its *absence*: the key is left
off that group's tags, so one line comes back scoped by the keys it does have.
With no `by`, every selected series is one group.

### 4. Space-aggregate

The aggregator before the `:` combines a group's series bucket by bucket,
ignoring nulls; a bucket where every series is null stays null.

`p50`…`p99` merge the bucket's **sketches** and then take the quantile — they
are not an average of per-series percentiles, which is not a percentile of
anything. They are refused on a metric that is not a distribution.

### 5. Modify

Modifiers apply in the order written, and each kind may appear once.

`.rollup()` is the exception to the numbering: it names the *time*
aggregation, so its method is applied at stage 2 and its width decides the
grid before a single sample is read. The other three apply to the finished
line, **after** the series have been combined.

That matters most for `fill`. Filling before the group were combined would
invent a reporting series: a host that sent nothing would count as a zero in
an `avg:`, dragging it down by exactly as much as the number of hosts that
were down. Filling afterwards only fills buckets where *nothing* reported,
which is the question the reader is asking.

| Modifier | Effect |
|---|---|
| `.rollup(method[, secs])` | override the time aggregation, and optionally the bucket width |
| `.as_rate()` | divide each bucket by its width — a count becomes per-second |
| `.as_count()` | the inverse — a rate becomes the count over the bucket |
| `.fill(mode[, secs])` | replace nulls: `zero`, `last`, `linear`, or `null` to keep them, optionally only within `secs` of real data |

`.as_rate()` and `.as_count()` are refused on a metric that is not a count or
a rate: the answer would be a number with no meaning. Because they run after
stage 4, they divide the *aggregated* line — which is the same number either
way for `sum` and `avg`, since both are linear.

A `.rollup(method, …)` on a percentile query keeps its width and reports a
warning that the method was not used: a percentile's buckets are merged
sketches, and a merge is the only reduction that preserves the error bound.

### Worked example

Two hosts serving one route, a `count` metric flushed every 10 seconds:

| t | 0 | 10 | 20 | 30 | 40 | 50 |
|---|---|---|---|---|---|---|
| `host:a,route:/x` | 1 | 2 | 3 | 4 | 5 | 6 |
| `host:b,route:/x` | 10 | 20 | 30 | 40 | 50 | 60 |

**Stage 2, time-aggregate** at `interval=30`. The metric is a count, so each
series' samples *sum* within each bucket:

| series | `[0,30)` | `[30,60)` |
|---|---|---|
| `host:a` | 1+2+3 = **6** | 4+5+6 = **15** |
| `host:b` | 10+20+30 = **60** | 40+50+60 = **150** |

**Stages 3–5, group and space-aggregate.** Both series are `route:/x`, so they
are one group, combined bucket by bucket:

| query | `[0,30)` | `[30,60)` |
|---|---|---|
| `sum:req.count{*} by {route}` | 6+60 = **66** | 15+150 = **165** |
| `avg:req.count{*} by {route}` | (6+60)/2 = **33** | (15+150)/2 = **82.5** |
| `max:req.count{*} by {route}` | **60** | **150** |
| `count:req.count{*} by {route}` | **2** | **2** |
| `sum:…{*} by {route}.as_rate()` | 66/30 = **2.2** | 165/30 = **5.5** |
| `avg:…{*} by {route}.rollup(max)` | (3+30)/2 = **16.5** | (6+60)/2 = **33** |

The last row is the one to read twice. `.rollup(max)` changes **stage 2**: each
series contributes its largest sample in the bucket — 3 for `host:a`, 30 for
`host:b` — and the `avg:` still runs across the two series afterwards.
Overriding the time aggregation does not move it.

Doing the two stages in the other order would give different numbers for every
row but `max`. `avg` space-first would be the mean of 11, 22, 33 = 22 rather
than 33, which weights each host by how often it happened to report rather than
by what it counted. That is why the order is fixed and not an option.

### Arithmetic and functions

`+ - * /` join two queries by matching their group tag sets: a group present
on one side and not the other is dropped, and a scalar broadcasts over every
group. Division by zero is null, not an error.

In the table below `q` is a query or any expression, `n` a plain number and
`"…"` a quoted keyword.

| Function | Meaning |
|---|---|
| `abs(q)`, `log2(q)`, `log10(q)` | per point |
| `clamp_min(q, n)`, `clamp_max(q, n)` | bound each point |
| `moving_avg(q, n)` | mean of the last `n` buckets |
| `diff(q)` | per-bucket change, for a gauge reporting a running total |
| `timeshift(q, secs)` | move `q`'s window, normally by a negative offset |
| `top(q, n[, "mean"\|"sum"\|"min"\|"max"\|"last"][, "asc"\|"desc"])` | keep `n` groups |
| `histogram_quantile(n, q)` | interpolate the quantile `n` from `q`'s cumulative `upper_bound` buckets — **the number comes first** |

`histogram_quantile` is the path for metrics scraped from a Prometheus
exporter, where buckets are all there is; `pXX` is the path for metrics that
arrive as sketches. Both exist so the same data can be measured two ways and
the error compared:

```
histogram_quantile(0.9, sum:http.request.duration.bucket{service:api} by {upper_bound})
p90:http.request.duration{service:api}
```

The quantile is the *first* argument, unlike every other function here, because
that is the order Prometheus uses and the one anybody porting a query will
type.

A number-shaped argument must be a plain number, possibly negated —
`clamp_min(q, 2*3)` is refused rather than folded.

## Limits

| Limit | Value | Why |
|---|---|---|
| query length | 8192 bytes | the parse runs before anything has judged the request |
| nesting depth | 32 | recursive descent uses the stack |
| values in one `IN` list | 128 | |
| a modifier's seconds | 1 … 2678400 (31 days) | zero would be indistinguishable from "not written" |

Limits on how much a query may *read* — series selected, buckets returned,
wall-clock — belong to the planner, not to the grammar.

## Canonical form

`Node.String()` prints one canonical spelling of a parsed query: minimal
parentheses, keys lower-cased, `IN` upper-cased, `{*}` for a query with no
matchers. Printing then parsing is the identity on the tree, which is what
lets the API echo a normalized `query` field and a dashboard be compared as
text. `parse(print(ast)) == ast` is a property test and a fuzz target.

Parentheses are kept on the **right** of any operator when the child is a
binary of equal precedence: the parser builds left-leaning trees, so
`a + (b + c)` without its parentheses would come back as `(a + b) + c` — a
different tree, and over float64 often a different number.

## Errors

A parse failure is a `*metricql.Error` with a `Col`, the 1-based byte column
the query editor underlines:

```
avg:http.request.duration{service:api by {route}
                                         ^ col 42: expected ',' or '}' but found '{'
```

## Examples

```
avg:http.request.duration{service:api,env:dev} by {route}

sum:http.request.count{service:api,status:5*}.as_rate()
  / sum:http.request.count{service:api}.as_rate() * 100

p95:judge.run.duration{language:python} by {problem_difficulty}.rollup(max, 60)

top(sum:container.cpu.usage{compose_project:demo} by {container_name}, 5, "mean", "desc")
```
