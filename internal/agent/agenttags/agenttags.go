package agenttags

import (
	"slices"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Decorate returns tags (already normalized) with the agent's global tags
// and host tag added, in canonical order and capped at wire.MaxTagsPerPoint.
// The host tag is added only when no tag has the key host — a series about
// another machine (tags: [host:db1], or a check that sets it) keeps its own.
// dropped is how many tags the cap removed; which ones is deterministic,
// because the set is sorted first, so a series keeps one identity.
func Decorate(tags, agentTags []string, hostTag string) (out []string, dropped int) {
	out = make([]string, 0, len(tags)+len(agentTags)+1)
	out = append(out, tags...)
	out = append(out, agentTags...)
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
