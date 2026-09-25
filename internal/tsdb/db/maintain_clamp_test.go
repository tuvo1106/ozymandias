package db

import (
	"errors"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// TestDB_CutBlockWillNotFreezeTheHeadAboveNow.
//
// The cut threshold bounds cutAt by the head's MaxT, and MaxT is not bounded
// by this process's clock: wire.MaxFutureSkew deliberately accepts samples up
// to ten minutes ahead, because clients' clocks are wrong and rejecting their
// data outright is worse. So one client running fast drags MaxT forward, and
// with a short block range cutAt lands in the future — at which point Freeze
// and Truncate set minValid above now and appendTo answers ErrOutOfBounds to
// every sample arriving *in real time*, until the clock catches up.
//
// With the default two-hour block range that cannot happen, which is why it
// went unnoticed. But block_range is operator-configurable, Validate only
// checks that it is positive, and the shipped deploy/ozyd.yaml invites
// shortening it for a test. A minute-long range plus one five-minute-fast
// client is minutes of a store that acknowledges nothing.
func TestDB_CutBlockWillNotFreezeTheHeadAboveNow(t *testing.T) {
	const blockRange = time.Minute
	db, fake, _ := open(t, Options{BlockRange: blockRange, Retention: -1})
	now := fake.Now().UnixMilli()

	// A normal client reporting up to a moment ago, and one whose clock is
	// fast by less than MaxFutureSkew — so intake accepted its samples, which
	// is the deliberate choice wire makes rather than dropping data over a
	// clock nobody controls.
	skew := int64(wire.MaxFutureSkew/time.Millisecond) / 2
	fill(t, db, ref("m", "host:ok"), now-3_000, 1_000, 3) // ..., now-1000
	fill(t, db, ref("m", "host:fast"), now+skew, 1_000, 121)

	// Cut the way maintain() does — until there is nothing left to take.
	// One cut is not enough to show this: the first takes the real client's
	// range and leaves the head holding only the skewed samples, and it is
	// the *next* one, computing cutAt from a MinT that is now in the future,
	// that freezes the head ahead of the clock.
	for i := 0; i < 8; i++ {
		err := db.CutBlock()
		if errors.Is(err, errNothingToCut) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// Whatever it decided, the head must still accept a sample written now.
	// That is the property; where exactly it drew the boundary is not.
	res, err := db.Append(ctx, []tsdb.SeriesSamples{{
		Series:  ref("m", "host:ok"),
		Samples: []tsdb.Sample{{T: now, V: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 1 {
		t.Errorf("a sample at the current time was refused after the cut: %+v", res.Rejected)
	}
	if mv := db.head.MinValidTime(); mv > now {
		t.Errorf("the head's minimum valid time is %d, %dms above now — every real-time "+
			"sample is out of bounds until the clock reaches it", mv, mv-now)
	}
}
