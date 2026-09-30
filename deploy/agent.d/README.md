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
| `app-python.yaml` | `collectors.docker.container_name_rewrite`: its judge sandboxes are one container name |

Checks and log sources arrive later in M3 and in M4.
