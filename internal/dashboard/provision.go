package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Row is a definition on its way into storage: the columns a database needs,
// plus the bytes it will not interpret.
//
// It exists so that this package does not import a storage package. A
// dashboard is a *definition*; where the rows live is somebody else's problem,
// and a domain type that knows about SQLite is a domain type that has to change
// when the SQLite does.
type Row struct {
	UID         string
	Title       string
	Description string
	Definition  []byte
}

// Store is the little that provisioning needs from a database: write this
// definition, keyed by uid, and say whether it was actually different.
//
// Narrow on purpose. The implementation (internal/meta, via a few lines of
// adapter) has a dozen methods, and depending on all of them here would mean a
// test of provisioning needed a SQLite file to say anything.
type Store interface {
	UpsertProvisionedDashboard(ctx context.Context, row Row, now time.Time) (changed bool, err error)
}

// ProvisionResult reports what a provisioning pass did, for the startup log
// and for the test that proves it.
type ProvisionResult struct {
	// Loaded is every file that produced a dashboard.
	Loaded int
	// Changed is how many of those were new or different. Zero on a restart
	// that changed nothing, which is the normal case and worth being able to
	// see.
	Changed int
	// Failed is the files that could not be used. Provisioning does not stop
	// for them — see [Provision].
	Failed int
}

// Provision reads every *.json file under each of paths and upserts it by uid.
//
// # Why one bad file does not stop the rest
//
// Provisioning runs during startup, and the directories come from config: an
// app repo mounts its own, so a file ozyd has never seen can appear because
// somebody deployed a different service. Refusing to start would make one
// team's typo an outage for everybody's monitoring — at the exact moment the
// monitoring is most wanted. So a file that cannot be parsed or validated is
// logged with its path and the reason, counted, and skipped. The dashboards
// that are fine are loaded.
//
// A *directory* that does not exist is also not an error, for the same reason
// in reverse: `provisioning.paths` naming a directory an app has not mounted
// yet is a configuration that will become correct, and it should not stop ozyd
// until it does. A path that exists but cannot be read is different — that is a
// permission problem somebody needs to know about — and is counted as a
// failure.
//
// Files are read in sorted order within a directory, and directories in the
// order configured, so that two files claiming one uid resolve the same way on
// every startup instead of by whatever order the filesystem felt like.
func Provision(ctx context.Context, store Store, paths []string, now time.Time, log *slog.Logger) ProvisionResult {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var res ProvisionResult
	for _, dir := range paths {
		files, err := jsonFiles(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Not yet mounted. Say so once, at Info: it is worth seeing when
			// you are wondering where your dashboards went, and it is not a
			// problem in itself.
			log.Info("provisioning: no such directory, skipping", "path", dir)
			continue
		case err != nil:
			log.Error("provisioning: cannot read directory", "path", dir, "err", err)
			res.Failed++
			continue
		}
		for _, file := range files {
			if err := provisionFile(ctx, store, file, now, &res); err != nil {
				// Deliberately Error and continue: see the doc comment.
				log.Error("provisioning: skipping a dashboard", "file", file, "err", err)
				res.Failed++
			}
		}
	}
	if res.Loaded > 0 || res.Failed > 0 {
		log.Info("provisioned dashboards",
			"loaded", res.Loaded, "changed", res.Changed, "failed", res.Failed)
	}
	return res
}

func provisionFile(ctx context.Context, store Store, file string, now time.Time, res *ProvisionResult) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	d, err := Parse(data)
	if err != nil {
		return err
	}
	// A uid is what "upsert by uid" is keyed on. Defaulting it to the filename
	// would look helpful and then bite: renaming the file would create a second
	// dashboard rather than update the first, and the old one would stay
	// forever with nobody sure whether it is still wanted.
	if d.UID == "" {
		return fmt.Errorf(`a provisioned dashboard needs a "uid": it is what the upsert is keyed on, so that editing this file updates the dashboard instead of adding another`)
	}
	changed, err := store.UpsertProvisionedDashboard(ctx, Row{
		UID:         d.UID,
		Title:       d.Title,
		Description: d.Description,
		Definition:  data,
	}, now)
	if err != nil {
		return err
	}
	res.Loaded++
	if changed {
		res.Changed++
	}
	return nil
}

// jsonFiles lists the .json files directly in dir, sorted.
//
// Not recursive, deliberately: a dashboards directory whose subdirectories are
// also scanned makes it impossible to keep a fixture, a README's example, or a
// work-in-progress next to the real ones without provisioning it.
func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}
