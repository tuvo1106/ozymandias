package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The connection string pgx is given. A review read the socket form,
// "[/var/run/postgresql]:5432", as one that never connects; pgx parses it
// back to the socket directory, and pgconn dials a unix socket for a host
// starting with "/". This pins that, and that a password with URL syntax
// in it stays a password.
func TestConnString(t *testing.T) {
	tcp, err := pgx.ParseConfig(connString(Config{Host: "db", Port: 6543, DBName: "app", SSLMode: "disable", User: "u", Password: "p@ss/w:rd"}))
	if err != nil {
		t.Fatal(err)
	}
	if tcp.Host != "db" || tcp.Port != 6543 || tcp.Database != "app" || tcp.User != "u" || tcp.Password != "p@ss/w:rd" {
		t.Fatalf("tcp: host %q port %d db %q user %q password %q", tcp.Host, tcp.Port, tcp.Database, tcp.User, tcp.Password)
	}
	sock, err := pgx.ParseConfig(connString(Config{Host: "/var/run/postgresql", Port: 5432, DBName: "app", SSLMode: "disable", User: "u", Password: "p"}))
	if err != nil {
		t.Fatal(err)
	}
	if sock.Host != "/var/run/postgresql" || sock.Port != 5432 || sock.Database != "app" || sock.User != "u" || len(sock.Fallbacks) != 0 {
		t.Fatalf("socket: host %q port %d db %q user %q", sock.Host, sock.Port, sock.Database, sock.User)
	}
	if network, addr := pgconn.NetworkAddress(sock.Host, sock.Port); network != "unix" || addr != "/var/run/postgresql/.s.PGSQL.5432" {
		t.Fatalf("socket dials %s %s", network, addr)
	}
	if sock.RuntimeParams["application_name"] != "ozymandias-agent" {
		t.Fatalf("application_name lost: %v", sock.RuntimeParams)
	}
}
