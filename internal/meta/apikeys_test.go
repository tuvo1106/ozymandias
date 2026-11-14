package meta

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAPIKeys_CreateListRevokeTouch(t *testing.T) {
	d := dashDB(t)
	ctx := context.Background()
	k, err := d.CreateAPIKey(ctx, "app", "aa11", "", t0)
	if err != nil || k.ID == 0 || k.LimitsJSON != "{}" {
		t.Fatalf("create: %+v %v", k, err)
	}
	if _, err := d.CreateAPIKey(ctx, "dup", "aa11", "{}", t0); err == nil {
		t.Fatal("two keys with one hash were stored")
	}
	rows, _ := d.APIKeys(ctx)
	if len(rows) != 1 || !rows[0].Active() || !rows[0].LastUsedAt.IsZero() {
		t.Fatalf("listing: %+v", rows)
	}
	if err := d.TouchAPIKey(ctx, k.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	first := t0.Add(2 * time.Hour)
	if err := d.RevokeAPIKey(ctx, k.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := d.RevokeAPIKey(ctx, k.ID, first.Add(time.Hour)); err != nil { // keeps the first time
		t.Fatal(err)
	}
	rows, _ = d.APIKeys(ctx)
	if rows[0].Active() || !rows[0].RevokedAt.Equal(first) || !rows[0].LastUsedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after revoke: %+v", rows[0])
	}
	if err := d.RevokeAPIKey(ctx, 999, t0); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("revoking an unknown id: %v", err)
	}
}
