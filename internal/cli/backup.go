package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/backup"
	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
)

// backupTimeout bounds the backup request. A backup copies the data; for a large
// data_dir that is minutes, so the bound is generous and the server is the one
// that decides when it is done.
const backupTimeout = 30 * time.Minute

// defaultAdminPasswordEnv matches internal/server: the variable the admin
// password is read from when the config does not name one.
const defaultAdminPasswordEnv = "OZYMANDIAS_ADMIN_PASSWORD"

// ozydBackup is `ozyd backup [-config FILE] [-name NAME]`: it asks the running
// ozyd on this host to make a backup under backup.root and prints where it is.
//
// It is a client of the running server, not a reader of data_dir: Pebble holds
// its directory open exclusively, and a copy of live files made from outside
// the process would not be consistent with what the process has in memory.
func ozydBackup(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ozyd backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "YAML config file (default: built-in defaults)")
	name := fs.String("name", "", "backup name (default: the current UTC time)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	cfg, _, err := config.LoadOzyd(*configPath, environ)
	if err != nil {
		fmt.Fprintf(stderr, "ozyd backup: %v\n", err)
		return ExitUsage
	}
	if cfg.Backup.Root == "" {
		fmt.Fprintln(stderr, "ozyd backup: backup.root is not set in the config; backups are off")
		return ExitUsage
	}
	url, err := httpserve.LoopbackURL(cfg.HTTP.Addr, "/api/v1/admin/backup")
	if err != nil {
		fmt.Fprintf(stderr, "ozyd backup: %v\n", err)
		return ExitUsage
	}
	body, _ := json.Marshal(map[string]string{"name": *name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "ozyd backup: %v\n", err)
		return ExitFailure
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Auth.Enabled {
		env := cfg.Auth.AdminPasswordEnv
		if env == "" {
			env = defaultAdminPasswordEnv
		}
		pw := lookupEnv(environ, env)
		if pw == "" {
			fmt.Fprintf(stderr, "ozyd backup: auth is on and $%s is empty\n", env)
			return ExitUsage
		}
		req.Header.Set("Authorization", "Bearer "+pw)
	}
	resp, err := (&http.Client{Timeout: backupTimeout}).Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "ozyd backup: is ozyd running at %s? %v\n", cfg.HTTP.Addr, err)
		return ExitFailure
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.Unmarshal(b, &e)
		fmt.Fprintf(stderr, "ozyd backup: %d %s\n", resp.StatusCode, firstNonEmpty(e.Error, strings.TrimSpace(string(b))))
		return ExitFailure
	}
	var out struct {
		Dir, Summary string
	}
	_ = json.Unmarshal(b, &out)
	fmt.Fprintf(stdout, "backup written to %s (%s)\n", out.Dir, out.Summary)
	return ExitOK
}

// ozydRestore is `ozyd restore [-config FILE] [-verify] BACKUP_DIR`: it checks
// the backup against its manifest and copies it into the config's data_dir,
// which must be empty. Offline: ozyd must not be running on that data_dir.
func ozydRestore(args, environ []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ozyd restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "YAML config file (default: built-in defaults)")
	verifyOnly := fs.Bool("verify", false, "check the backup against its manifest and stop")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: ozyd restore [-config FILE] [-verify] BACKUP_DIR")
		return ExitUsage
	}
	src := fs.Arg(0)
	if *verifyOnly {
		m, err := backup.Verify(src)
		if err != nil {
			fmt.Fprintf(stderr, "ozyd restore: %v\n", err)
			return ExitFailure
		}
		fmt.Fprintf(stdout, "backup ok: %s\n", m.Summary())
		return ExitOK
	}
	cfg, _, err := config.LoadOzyd(*configPath, environ)
	if err != nil {
		fmt.Fprintf(stderr, "ozyd restore: %v\n", err)
		return ExitUsage
	}
	m, err := backup.Restore(src, cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "ozyd restore: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(stdout, "restored %s into %s; start ozyd to recover it\n", m.Summary(), cfg.DataDir)
	return ExitOK
}

func lookupEnv(environ []string, key string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
