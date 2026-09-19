# M6 — Monitors and alerting

**Goal:** define monitors over metrics and logs, evaluate them continuously,
track per-group alert state with hysteresis and no-data detection, and notify
via webhook / Discord / email.

**Concepts learned:** alert evaluation windows and delays (why you never
evaluate "now"); state machines and flapping; hysteresis via recovery
thresholds; multi-alerts (one monitor, state per group); no-data vs zero;
notification dedup/renotify; why alerting needs its own reliability story.

## 1. Monitor definition

Metadata tables: `monitors(id, uid, name, type, query, options_json, message, tags_json, enabled, muted_until, created_at, updated_at)`,
`monitor_states(monitor_id, group_key, status, since_ts, last_value, last_eval_ts, last_data_ts, last_notified_ts, PRIMARY KEY(monitor_id, group_key))`,
`events(id, ts, kind, monitor_id, group_key, from_status, to_status, value, text)`.

```json
{
  "uid": "app-python-judge-backlog",
  "name": "Judge queue is backing up",
  "type": "metric",
  "query": "avg(last_5m):avg:arq.queue.depth{service:app-python-api} > 20",
  "options": {
    "thresholds": {"critical": 20, "warning": 10, "critical_recovery": 15, "warning_recovery": 8},
    "evaluation_interval_s": 30,
    "evaluation_delay_s": 30,
    "require_full_window": false,
    "notify_no_data": true, "no_data_timeframe_m": 10,
    "renotify_interval_m": 60,
    "min_consecutive_failures": 1
  },
  "message": "Queue depth is {{.Value}} (threshold {{.Threshold}}) for {{.Group}}.\n{{if .IsAlert}}Check worker health.{{end}} @discord-ops",
  "tags": ["team:me", "app:app-python"]
}
```

### Monitor query grammar (added to `docs/query-language.md`)

```
metric_monitor = time_agg "(" "last_" duration ")" ":" metricql_query op number ;
time_agg       = "avg" | "sum" | "min" | "max" | "last" | "pct_nonnull" ;
log_monitor    = "logs(" quoted_logql ")" ".rollup(\"count\")" [ ".by(" keys ")" ] ".last(" duration ")" op number ;
op             = ">" | ">=" | "<" | "<=" | "==" | "!=" ;   duration = int ("m"|"h") ;
```
A `by {…}` in the metric query makes it a **multi-alert**: independent state
per group. The comparison threshold in the query must equal
`thresholds.critical` (validated).

## 2. Evaluator — `internal/monitor`

- **Scheduler:** each enabled monitor runs every `evaluation_interval_s`,
  start offset jittered by hash(uid); a bounded worker pool (4) executes
  evaluations; an evaluation that overruns its interval skips the next tick
  and counts `ozy.monitor.eval_skipped`. Injectable `Clock`.
- **Evaluation:** window = `[now − delay − last_N, now − delay]` (delay lets
  10s agent buckets + forwarding land). Run the metricql query (interval =
  native 10s or rollup), reduce each group's points with `time_agg` →
  scalar. `require_full_window` → a group with nulls in the window is
  skipped. Log monitors call `LogStore.Aggregate` count (optionally by facet).
- **State machine** per `(monitor, group)`: `OK | WARN | ALERT | NO_DATA`
  (+ monitor-level `MUTED` flag that suppresses notifications, not evaluation).

  | From | Condition | To |
  |---|---|---|
  | OK | value breaches warning | WARN |
  | OK/WARN | value breaches critical for `min_consecutive_failures` evals | ALERT |
  | ALERT | value past `critical_recovery` (defaults to critical) but still breaching warning | WARN |
  | ALERT/WARN | value past `warning_recovery` / `critical_recovery` and not breaching | OK |
  | any | group had data before, none for `no_data_timeframe_m`, `notify_no_data` | NO_DATA |
  | NO_DATA | data returns | evaluate normally |

  "Breaches" respects the operator direction (for `<` monitors, recovery
  thresholds are *above*). Groups unseen for 24h are garbage-collected.
- **Persistence:** state rows updated transactionally with the emitted event;
  on restart the evaluator resumes from the DB (no re-notification for
  already-notified states; renotify timers continue from `last_notified_ts`).
- **Notifications:** on a status transition, and every `renotify_interval_m`
  while in ALERT/NO_DATA. Recovery notifications on → OK. Rendered with Go
  `text/template`; data: `Monitor, Group (map + string), Status, PrevStatus,
  Value, Threshold, IsAlert, IsWarning, IsRecovery, IsNoData, Since, Link`
  (link to the UI monitor page with the time window). `@handle` mentions in
  the message select notifiers.
- **Notifiers** (`internal/monitor/notify`, interface
  `Notify(ctx, Notification) error`), configured in `ozyd.yaml`:

  ```yaml
  notifiers:
    discord-ops: {type: discord, webhook_url_env: DISCORD_WEBHOOK_URL}
    hook:        {type: webhook, url: http://host.docker.internal:9999/alert, headers: {…}}
    me:          {type: email, to: [you@example.com], smtp: {host_env: SMTP_HOST, port: 587, user_env: SMTP_USER, password_env: SMTP_PASSWORD, from: ozymandias@localhost}}
    log:         {type: log}
  ```
  Secrets only via env indirection, never stored in SQLite or returned by the
  API. Delivery: 3 attempts with backoff, 10s timeout, outcome recorded as an
  event (`notify_ok` / `notify_failed`); a failing notifier never blocks
  evaluation (own queue + worker).
- **Self-watchdog:** `ozy.monitor.last_eval_age_seconds` gauge; a
  built-in monitor-of-monitors logs loudly and shows a banner in the UI if
  evaluations stop.

## 3. API

`GET/POST /api/v1/monitors`, `GET/PUT/DELETE /api/v1/monitors/{id}`,
`POST /api/v1/monitors/validate` (parses query, checks thresholds, dry-runs
the evaluation and returns current per-group values),
`POST /api/v1/monitors/{id}/mute {until}` / `unmute`,
`GET /api/v1/monitors/{id}/states`, `GET /api/v1/events?monitor_id=&from=&to=`,
`POST /api/v1/monitors/{id}/test-notification`.
Provisioning from `deploy/monitors/*.json` (upsert by `uid`), like dashboards.

## 4. UI
Monitors list (status pills, group counts by state, mute, filter by tag/status);
create/edit form with live preview chart — query line, threshold bands,
evaluation-window shading, would-have-alerted markers over the last day
(backtest via the same evaluator with a simulated clock); monitor detail
(per-group state table, status timeline, event history, notification log);
events overlay toggle on dashboard timeseries widgets (vertical markers at
transitions); a triggered-monitors badge in the nav.

## 5. Shipped monitors (`deploy/monitors/`)
- app-python: judge queue backlog (above); `p95:trace.http.request.duration{service:app-python-api} by {resource} > 1` s;
  API 5xx rate > 2%; `logs("service:app-python-worker status:error").rollup("count").last("5m") > 5`;
  judge sandbox OOM exits > 0; worker no-data (heartbeat metric absent 3m).
- app-node: 5xx count > 0 over 10m; `data/` disk usage gauge > 80%
  (emitted by a tiny SDK gauge on a timer — see integrations.md);
  Metron `burstRemaining` < 3 (from its existing log field via a log monitor
  or a gauge).
- app-ruby (all on scraped metrics): queue depth high for 5m; small-order p90 wait above its §10.4 target; `diff(app-ruby.quality_breaches)` > 0 over 15m; ETA bias drifting positive; Sidekiq queue latency; `openmetrics.up` = 0 (scrape failing).
- ozymandias self: forwarder drops > 0; WAL fsync p99 > 200 ms; head series > 80% of limit;
  log ingestion silent 10m.

## 6. Test plan

- **L1** monitor-query parser (all forms + errors); threshold validation
  (direction-aware: recovery on the correct side; warning between recovery and
  critical); `time_agg` reducers with nulls; `require_full_window`; the full
  state-transition table for `>` and `<` monitors, incl. hysteresis (value
  oscillating between recovery and threshold does **not** flap),
  `min_consecutive_failures`, NO_DATA entry/exit, never-seen group is not
  NO_DATA, group GC; renotify timing; mute suppresses notify but state still
  advances; template rendering + bad template rejected at save; mention →
  notifier routing; notifier payload shapes (Discord embed JSON, webhook JSON,
  email MIME) and retry/backoff; secrets never in API responses; provisioning
  upsert; scheduler jitter, overrun skip, pool bound.
- **L2** state machine: for any value sequence, (a) notifications fire only on
  transitions/renotify, (b) status is a pure function of (previous state,
  value, options) — replaying the same sequence yields the same events,
  (c) no transition skips persistence.
- **L4** fuzz: monitor-query parser; template compile with arbitrary input.
- **L5** restart mid-ALERT: no duplicate notification, renotify continues;
  DB write failure during transition → no notification sent without persisted
  state (and vice-versa converges on next eval); notifier endpoint down /
  slow / 500 → evaluation cadence unaffected; query timeout → monitor
  evaluation error event, state unchanged.
- **L6** 200 monitors × 50 groups on the fake clock under `-race`; leak check.
- **L7** goldens: notification payloads; API responses.
- **L8** fake-clock scenario tests end-to-end in-process: feed a metric
  timeline (ok → warn → alert → hover in hysteresis band → recover → silence
  → no-data → return) and assert the exact event + notification sequence via
  the `log`/capture notifier. Backtest endpoint returns the same transitions.
- **L9** smoke v6: provision a monitor on `smoke.gauge > 5`, send 10 → webhook
  receiver (tiny listener in the smoke script) gets ALERT; send 0 → gets RECOVERY.
- **L11** threshold-band chart math; form validation; Playwright create → trigger → see state.

## 7. Docs deliverables
`docs/query-language.md` monitor grammar; `docs/api.md` monitors/events;
`docs/operations.md`: notifier config, secrets handling, provisioning,
troubleshooting "why didn't my monitor fire" (delay, window, no-data vs zero,
counter zero-fill from M1); DESIGN.md: evaluation model with a timeline
diagram (window, delay, agent flush), state machine diagram
(`monitor-state-machine.mmd`), notification reliability; ADRs: evaluation
delay, state in SQLite, Go templates; `docs/notes/M6.md`.

## 8. Acceptance criteria

- [ ] Flooding app-python with submissions (script in `scripts/demo/`) fires the backlog monitor to Discord/webhook within `window + delay + interval`, then recovers on its own with one recovery message.
- [ ] A value oscillating inside the hysteresis band produces zero extra notifications (scenario test + live demo).
- [ ] Stopping the app-python worker triggers the no-data monitor; restarting clears it.
- [ ] Restarting ozyd during an active alert sends no duplicate.
- [ ] The log monitor fires on injected worker errors.
- [ ] The editor's backtest markers match what the live evaluator then does.
- [ ] Tests and docs deliverables complete; `docs/notes/M6.md` has evidence.
