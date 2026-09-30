// Package postgres is the postgres check: a PostgreSQL server's
// connections, transaction and row activity, buffer cache hit ratio, and
// database, table and index sizes, reported as postgresql.*
// (docs/metrics-catalog.md).
//
// # Where the numbers come from
//
// Postgres keeps cumulative statistics per database in pg_stat_database —
// commits, rollbacks, rows returned and written, blocks found in shared
// buffers and blocks read from disk — since the statistics were last reset.
// Each run connects (pgx, ADR-0031), reads them, and disconnects; the check
// turns two runs' counters into per-second rates with collector.Rates, which
// skips a run across pg_stat_reset() rather than reporting a negative spike.
// Connections are the client sessions in pg_stat_activity (not the
// server's background processes, which it also lists since PostgreSQL 10
// and which max_connections does not count), sizes from pg_database_size,
// pg_table_size and pg_indexes_size.
//
// # Choices worth knowing
//
//   - postgresql.buffer_hit is the share of block reads served from shared
//     buffers over the last interval, from the two block rates — not the
//     lifetime ratio, which on a long-running server barely moves however
//     bad the last minute was.
//   - One connection per run, closed afterwards: nothing to keep healthy
//     between runs, and a restarted or failed-over server costs nothing to
//     recover from. The check's own connection is counted in
//     postgresql.connections.
//   - postgresql.can_connect is emitted on every run, 1 or 0, so "the
//     server is down" is a value on a chart rather than an absence.
//   - Template databases, and those that allow no connections, are left out.
//     A database's size needs CONNECT privilege on it; without it the size
//     is not reported rather than failing the run.
//   - Every series carries server:<host> and port:<port>, so two instances
//     never write the same series.
//
// # Settings
//
// host (required), port (5432; a numeric string is accepted, as container
// labels are strings), user, password (never logged), dbname (postgres),
// sslmode (disable), timeout (5s, for the whole run), relations (tables whose
// sizes to report, at most MaxRelations).
package postgres
