# agent.d — per-app agent configuration

Each `*.yaml` file here is a **fragment** merged over `deploy/agent.yaml` in
file-name order. Only `.yaml`/`.yml` files are read; this README is ignored.

- Maps merge, lists append, and a scalar set in two files is an error that
  names both files.
- A fragment may use any key `agent.yaml` accepts, except `confd_path`.
- One file per app (`<app>.yaml`). This directory, `deploy/dashboards/` and
  `deploy/monitors/` are the only places app-specific configuration lives
  (docs/plan/extensibility.md §1).

| File | What it sets |
|---|---|
| `logs.yaml` | `logs.enabled` and the registry path: log collection is on in the compose stack, container logs by label |
| `app-python.yaml` | `collectors.docker.container_name_rewrite`: its judge sandboxes are one container name |

Log sources go in the app's own fragment, as `logs.sources` entries (lists append across
fragments). A file source reads under `/var/log/apps` (mount your host's log directory with
`OZY_APP_LOGS`); a container opts in with the label `ozy.logs.enabled=true`, and may carry its
own `ozy.logs.source`, `ozy.logs.service`, `ozy.logs.multiline_start` and `ozy.logs.tags`, so a
container needs no fragment at all. See docs/operations.md, "Logs".

