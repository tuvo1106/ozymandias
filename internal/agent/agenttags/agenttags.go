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
func DecorateKeeping(own, keep, agentTags []string, hostTag string) (out []string, dropped int) {
	if len(keep) == 0 {
		return Decorate(own, agentTags, hostTag)
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
