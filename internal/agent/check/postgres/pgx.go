package postgres

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// connectionsQuery counts client sessions. Since PostgreSQL 10,
// pg_stat_activity also lists the server's background processes
// (checkpointer, WAL writer, autovacuum launcher, …), which do not count
// against max_connections: counting them made percent_usage_connections
// read 40% on a server with 3 of 20 slots in use. Before 10 the view has no
// backend_type and lists client sessions only, so connectionsQueryOld is
// the same answer there.
const (
	connectionsQuery    = `SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'`
	connectionsQueryOld = `SELECT count(*) FROM pg_stat_activity`
	undefinedColumn     = "42703" // SQLSTATE
)

// dialPgx connects with pgx (ADR-0031). The connection string is built as a
// URL so that a password with '@' or '/' in it is escaped, not parsed.
func dialPgx(ctx context.Context, cfg Config) (conn, error) {
	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))),
		Path:     "/" + cfg.DBName,
		RawQuery: url.Values{"sslmode": {cfg.SSLMode}, "application_name": {"ozymandias-agent"}}.Encode(),
	}
	if cfg.User != "" {
		u.User = url.UserPassword(cfg.User, cfg.Password)
	}
	pc, err := pgx.ParseConfig(u.String())
	if err != nil {
		// pgconn's parse errors redact the password; the URL itself is
		// never put in an error here.
		return nil, err
	}
	c, err := pgx.ConnectConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	return pgxConn{c}, nil
}

type pgxConn struct{ c *pgx.Conn }

func (p pgxConn) Close(ctx context.Context) error { return p.c.Close(ctx) }

func (p pgxConn) Connections(ctx context.Context) (n int64, err error) {
	err = p.c.QueryRow(ctx, connectionsQuery).Scan(&n)
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == undefinedColumn {
		err = p.c.QueryRow(ctx, connectionsQueryOld).Scan(&n) // before PostgreSQL 10
	}
	return n, err
}

func (p pgxConn) MaxConnections(ctx context.Context) (int64, error) {
	var s string
	if err := p.c.QueryRow(ctx, `SHOW max_connections`).Scan(&s); err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

// Databases reads pg_stat_database for every database a client could
// connect to. The size needs CONNECT privilege on that database, so it is
// NULL (and not reported) where the check's user lacks it.
func (p pgxConn) Databases(ctx context.Context) ([]Database, error) {
	rows, err := p.c.Query(ctx, `
SELECT d.datname, s.xact_commit, s.xact_rollback, s.tup_returned, s.tup_fetched,
       s.tup_inserted, s.tup_updated, s.tup_deleted, s.deadlocks, s.temp_bytes,
       s.blks_hit, s.blks_read,
       CASE WHEN has_database_privilege(d.oid, 'CONNECT') THEN pg_database_size(d.oid) END
  FROM pg_stat_database s JOIN pg_database d ON d.oid = s.datid
 WHERE NOT d.datistemplate AND d.datallowconn
 ORDER BY d.datname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Database, error) {
		var db Database
		var size *int64
		err := r.Scan(&db.Name, &db.Commits, &db.Rollbacks, &db.Returned, &db.Fetched,
			&db.Inserted, &db.Updated, &db.Deleted, &db.Deadlocks, &db.TempBytes,
			&db.BlocksHit, &db.BlocksRead, &size)
		if size != nil {
			db.Size, db.SizeKnown = *size, true
		}
		return db, err
	})
}

func (p pgxConn) Relations(ctx context.Context, names []string) ([]Relation, error) {
	rows, err := p.c.Query(ctx, `
SELECT n.nspname, c.relname, pg_table_size(c.oid), pg_indexes_size(c.oid)
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p') AND c.relname = ANY($1)
 ORDER BY n.nspname, c.relname`, names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Relation, error) {
		var rel Relation
		err := r.Scan(&rel.Schema, &rel.Name, &rel.TableSize, &rel.IndexSize)
		return rel, err
	})
}
