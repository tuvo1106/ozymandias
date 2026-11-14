package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/server"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ozyd.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(args ...string) (int, string, string) {
	var out, errb syncBuffer
	code := Ozyd(context.Background(), args, nil, &out, &errb, nil)
	return code, out.String(), errb.String()
}

func TestBackupCLI_BackupThenRestoreThroughTheRealCommands(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	root := t.TempDir()
	ln, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfig(t, fmt.Sprintf("http:\n  addr: %q\ndata_dir: %q\nbackup:\n  root: %q\n", ln.Addr().String(), data, root))
	cfg, _, err := config.LoadOzyd(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.New(cfg, server.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	testutil.Eventually(t, 5*time.Second, func() bool {
		return httpserve.Probe(ctx, "http://"+ln.Addr().String()+"/healthz", time.Second) == nil
	}, "ozyd never came up")

	code, out, errs := run("backup", "-config", cfgPath, "-name", "nightly")
	if code != ExitOK || !strings.Contains(out, filepath.Join(root, "nightly")) {
		t.Fatalf("backup: %d out=%q err=%q", code, out, errs)
	}
	if code, _, errs := run("backup", "-config", cfgPath, "-name", "nightly"); code != ExitFailure || !strings.Contains(errs, "already exists") {
		t.Fatalf("a second backup under one name: %d %q", code, errs)
	}
	cancel()
	<-done

	if code, out, errs := run("restore", "-verify", filepath.Join(root, "nightly")); code != ExitOK || !strings.Contains(out, "backup ok") {
		t.Fatalf("verify: %d %q %q", code, out, errs)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	freshCfg := writeConfig(t, fmt.Sprintf("data_dir: %q\n", fresh))
	if code, out, errs := run("restore", "-config", freshCfg, filepath.Join(root, "nightly")); code != ExitOK || !strings.Contains(out, "restored") {
		t.Fatalf("restore: %d %q %q", code, out, errs)
	}
	if code, _, errs := run("restore", "-config", freshCfg, filepath.Join(root, "nightly")); code != ExitFailure || !strings.Contains(errs, "not empty") {
		t.Fatalf("a second restore into the same data_dir: %d %q", code, errs)
	}
}

func TestBackupCLI_RefusesWhatItShould(t *testing.T) {
	off := writeConfig(t, "data_dir: /tmp/x\n")
	if code, _, errs := run("backup", "-config", off); code != ExitUsage || !strings.Contains(errs, "backup.root") {
		t.Errorf("backups off: %d %q", code, errs)
	}
	authOn := writeConfig(t, "data_dir: /tmp/x\nbackup:\n  root: /tmp/y\nauth:\n  enabled: true\n")
	if code, _, errs := run("backup", "-config", authOn); code != ExitUsage || !strings.Contains(errs, "OZYMANDIAS_ADMIN_PASSWORD") {
		t.Errorf("auth on, no password: %d %q", code, errs)
	}
	if code, _, _ := run("restore"); code != ExitUsage {
		t.Errorf("restore with no argument: %d", code)
	}
	bad := t.TempDir()
	if code, _, errs := run("restore", "-verify", bad); code != ExitFailure || !strings.Contains(errs, "manifest") {
		t.Errorf("verify of a directory that is no backup: %d %q", code, errs)
	}
	nested := writeConfig(t, "data_dir: /tmp/x\nbackup:\n  root: /tmp/x/inside\n")
	if code, _, errs := run("backup", "-config", nested); code != ExitUsage || !strings.Contains(errs, "contain one another") {
		t.Errorf("a backup root inside data_dir: %d %q", code, errs)
	}
}
