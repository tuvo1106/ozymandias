# agent.d — per-app agent configuration

Each `*.yaml` file here is a **fragment** merged over `deploy/agent.yaml` in
file-name order. Only `.yaml`/`.yml` files are read; this README is ignored.

- Maps merge, lists append, and a scalar set in two files is an error that
  names both files.
- A fragment may use any key `agent.yaml` accepts, except `confd_path`.
- One file per app (`<app>.yaml`). This directory, `deploy/dashboards/` and
  `deploy/monitors/` are the only places app-specific configuration lives
  (docs/plan/extensibility.md §1).

Nothing app-specific exists yet. In M0, `tags` is the only list a fragment can
usefully extend. Checks, log sources and rewrite rules arrive in M3 and M4.
