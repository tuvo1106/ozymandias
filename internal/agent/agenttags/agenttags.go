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
