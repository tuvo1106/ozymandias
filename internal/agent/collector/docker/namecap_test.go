package docker

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestNameCap_AdmitsUpToTheLimitThenFolds(t *testing.T) {
	folds := 0
	c := NewNameCap(2, func() { folds++ })
	for _, n := range []string{"a", "b"} {
		if got := c.Admit(n); got != n {
			t.Fatalf("Admit(%q) = %q, want it admitted", n, got)
		}
	}
	if got := c.Admit("c"); got != OverflowName {
		t.Fatalf("a third name = %q, want %q", got, OverflowName)
	}
	if folds != 1 {
		t.Fatalf("folds = %d, want 1", folds)
	}
	// An admitted name stays admitted however many others were turned away,
	// or a name's series would change identity mid-life.
	if got := c.Admit("a"); got != "a" {
		t.Fatalf("an admitted name came back as %q", got)
	}
	if folds != 1 {
		t.Fatalf("an admitted name counted as a fold: %d", folds)
	}
}

func TestNameCap_NilAndZeroAreUnlimited(t *testing.T) {
	var nilCap *NameCap
	for _, c := range []*NameCap{nilCap, NewNameCap(0, nil)} {
		for i := 0; i < 1000; i++ {
			n := fmt.Sprintf("n%d", i)
			if got := c.Admit(n); got != n {
				t.Fatalf("unlimited cap changed %q to %q", n, got)
			}
		}
	}
}

func TestNameCap_ConcurrentAdmitsNeverExceedTheLimit(t *testing.T) {
	c := NewNameCap(10, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := map[string]bool{}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				n := fmt.Sprintf("n%d", i)
				if c.Admit(n) == n {
					mu.Lock()
					admitted[n] = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if len(admitted) != 10 {
		t.Fatalf("%d names admitted, want exactly 10", len(admitted))
	}
}

// What the cap is for: the tags a container gets. A folded container is one
// thing, so it loses its container_id exactly as a rewritten one does, and a
// rewritten name does not use a slot (the rule already said "one thing").
func TestTagger_CapFoldsOverflowNamesAndDropsTheirID(t *testing.T) {
	tg := tagger{
		rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-.*`), Replace: "judge"}},
		cap:      NewNameCap(1, nil),
	}
	tagsOf := func(name, id string) []string { return tg.tags(name, id, "img:1", nil) }

	if got := tagsOf("first", "aaaaaaaaaaaaaaaa"); !slices.Contains(got, "container_name:first") || !slices.Contains(got, "container_id:aaaaaaaaaaaa") {
		t.Fatalf("first name should be kept with its id: %v", got)
	}
	got := tagsOf("second", "bbbbbbbbbbbbbbbb")
	if !slices.Contains(got, "container_name:"+OverflowName) {
		t.Fatalf("over the cap should fold: %v", got)
	}
	for _, tag := range got {
		if len(tag) > 13 && tag[:13] == "container_id:" {
			t.Fatalf("a folded container kept its id: %v", got)
		}
	}
	// A rewritten name neither takes a slot nor is folded, even when full.
	// An empty name takes no slot either.
	if got := tagsOf("", "dddddddddddddddd"); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "container_name:") }) {
		t.Fatalf("an empty name got a name tag: %v", got)
	}
	if got := tagsOf("judge-123", "cccccccccccccccc"); !slices.Contains(got, "container_name:judge") {
		t.Fatalf("a rewritten name must not be capped: %v", got)
	}
}

// The claim "a rewritten name does not use a slot", pinned from the other
// side: a rewritten container seen first must leave the one slot free, and an
// empty name must too.
func TestTagger_RewrittenAndEmptyNamesLeaveTheSlotFree(t *testing.T) {
	tg := tagger{
		rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-.*`), Replace: "judge"}},
		cap:      NewNameCap(1, nil),
	}
	tg.tags("judge-1", "aaaaaaaaaaaaaaaa", "img:1", nil)
	tg.tags("", "bbbbbbbbbbbbbbbb", "img:1", nil)
	if got := tg.tags("real", "cccccccccccccccc", "img:1", nil); !slices.Contains(got, "container_name:real") {
		t.Fatalf("the slot was spent before a plain name asked: %v", got)
	}
}
