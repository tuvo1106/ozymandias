package agenttags

import (
	"slices"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Normalize returns the agent's global tags as Decorate expects them:
// normalized, with any that cannot be sent left out. Config validation
// refuses such a tag first; this is the backstop for a caller that skipped
// it, and the one place both the aggregator and the scheduler get the list
// from, so the two cannot clean it differently.
func Normalize(tags []string) []string {
	var out []string
	for _, t := range tags {
		if n, ok := wire.NormalizeTag(t); ok {
			out = append(out, n)
		}
	}
	return out
}

// Decorate returns tags (already normalized) with the agent's global tags
// and host tag added, in canonical order and capped at wire.MaxTagsPerPoint.
// The host tag is added only when no tag has the key host — a series about
// another machine (tags: [host:db1], or a check that sets it) keeps its own.
// dropped is how many tags the cap removed; which ones is deterministic,
// because the set is sorted first, so a series keeps one identity.
//
// Decorate appends to tags and may reorder it in place, so the caller
// passes a slice it owns, ideally with room for len(agentTags)+1 more: on
// the statsd path that saves a copy per sample (the caller has just built
// the slice from the raw tags anyway).
//
// DecorateKeeping is the variant for a metric some of whose tags must
// survive the cap.
func Decorate(tags, agentTags []string, hostTag string) (out []string, dropped int) {
	out = append(tags, agentTags...)
	if hostTag != "" && !wire.HasTagKey(out, "host") {
		out = append(out, hostTag)
	}
	out = wire.CanonicalTags(out)
	if len(out) > wire.MaxTagsPerPoint {
		dropped = len(out) - wire.MaxTagsPerPoint
		out = out[:wire.MaxTagsPerPoint]
	}
	return slices.Clip(out), dropped
}

// DecorateKeeping is Decorate for a series whose tags come in two kinds:
// own, what the source reported (a scraped page's labels, which may be
// many), and keep, the tags that say whose series it is (the check
// instance, its container, its replica). Decorate's cap cuts the sorted
// set, so it removes whichever tags sort last — a replica tag behind fifty
// scraped labels — and two series that differed only there would collide.
// Here the cap trims own tags first: keep, the agent's tags, the host tag
// and any own host tag are kept, and own tags fill what room is left, in
// sorted order so the same ones always survive.
//
// An own tag whose key a keep tag or an agent tag also has, with another
// value, is renamed exported_<key>, as
// Prometheus renames a scraped label that clashes with a target label: a
// page's service="checkout" beside the container's service:shop-api would
// otherwise give one series two values for one key, and a group-by on
// that key would count it in both groups. host is the exception: a check
// that reports another machine says so with its own host tag.
func DecorateKeeping(own, keep, agentTags []string, hostTag string) (out []string, dropped int) {
	// The agent's own tags clash as much as the instance's: env="staging"
	// on a page beside the agent's env:prod is two values for env.
	own, renamed := exportClashes(own, append(slices.Clip(keep), agentTags...))
	defer func() { dropped += renamed }()
	if len(keep) == 0 || len(own)+len(keep)+len(agentTags)+1 <= wire.MaxTagsPerPoint {
		// Nothing to choose between: everything fits, so this is
		// Decorate, without the set and the extra sorts below. The common
		// case, on the scheduler's per-metric path.
		return Decorate(append(own, keep...), agentTags, hostTag)
	}
	fixed := make([]string, 0, len(keep)+len(agentTags)+2)
	fixed = append(append(fixed, keep...), agentTags...)
	for _, t := range own {
		if k, _ := wire.SplitTag(t); k == "host" {
			fixed = append(fixed, t)
		}
	}
	if hostTag != "" && !wire.HasTagKey(fixed, "host") {
		fixed = append(fixed, hostTag)
	}
	fixed = wire.CanonicalTags(fixed)
	if len(fixed) > wire.MaxTagsPerPoint {
		dropped = len(fixed) - wire.MaxTagsPerPoint
		fixed = fixed[:wire.MaxTagsPerPoint]
	}
	in := make(map[string]bool, len(fixed))
	for _, t := range fixed {
		in[t] = true
	}
	var rest []string
	for _, t := range wire.CanonicalTags(slices.Clone(own)) {
		if !in[t] {
			rest = append(rest, t)
		}
	}
	if room := wire.MaxTagsPerPoint - len(fixed); len(rest) > room {
		dropped += len(rest) - room
		rest = rest[:room]
	}
	return wire.CanonicalTags(append(fixed, rest...)), dropped
}

// exportClashes renames each own tag (but host) whose key a keep tag has
// with another value: key:v becomes exported_key:v. One the longer key makes unsendable is
// dropped, and counted with the cap's drops.
func exportClashes(own, keep []string) (out []string, dropped int) {
	if len(keep) == 0 || len(own) == 0 {
		return own, 0
	}
	keys := make(map[string]bool, len(keep))
	same := make(map[string]bool, len(keep))
	for _, t := range keep {
		k, _ := wire.SplitTag(t)
		keys[k], same[t] = true, true
	}
	out = own[:0]
	for _, t := range own {
		// The same tag twice is one tag, not a clash.
		if k, v := wire.SplitTag(t); k != "host" && keys[k] && !same[t] {
			n, ok := wire.NormalizeTag(wire.JoinTag("exported_"+k, v))
			if !ok {
				dropped++
				continue
			}
			t = n
		}
		out = append(out, t)
	}
	return out, dropped
}
