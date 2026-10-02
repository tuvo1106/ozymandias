package docker

import (
	"regexp"

	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
)

// Labels the collector reads. Compose sets the first two on every container
// it creates; the third is ours, for a service name compose cannot express.
const (
	LabelComposeProject = "com.docker.compose.project"
	LabelComposeService = "com.docker.compose.service"
	LabelService        = "ozy.service"
)

// Rewrite renames containers whose name matches Match to Replace, before
// the name becomes a tag. The whole name is replaced, whatever part of it
// Match matched: `^judge-` with Replace `judge` renames judge-8f3a to judge.
// Replace is a Go regexp template, so ${1} is the match's first group; that
// is how a rule keeps a name (`^(ozy-smoke-[a-z]+)$` → `${1}`) while still
// dropping its id.
//
// It exists for containers that are many and short-lived by design — a judge
// sandbox per submission — where tagging each by its own name would create a
// series per container and bury the metric under them. A rewritten
// container also loses its container_id tag, which would defeat the point:
// the rewrite says "these are one thing", and the id says the opposite.
type Rewrite struct {
	Match   *regexp.Regexp
	Replace string
}

// tagger turns a container's identity into tags.
type tagger struct{ rewrites []Rewrite }

// tags returns the tags for a container, from its name, id, image reference
// and labels:
//
//	container_name, container_id (short; not for a rewritten name),
//	image_name, image_tag (not for a digest or bare id), compose_project,
//	compose_service, service (label ozy.service, else project-service).
//
// A tag whose value is empty is left out rather than sent as "key:".
func (t tagger) tags(name, id, image string, labels map[string]string) []string {
	out := make([]string, 0, 8)
	add := func(k, v string) {
		if v != "" {
			out = append(out, k+":"+v)
		}
	}
	rewritten := false
	for _, r := range t.rewrites {
		if m := r.Match.FindStringSubmatchIndex(name); m != nil {
			// A replacement that expands to nothing (${2} of a pattern with
			// one group, a group that matched empty) would leave the
			// container with neither name nor id: keep the name instead.
			if n := string(r.Match.ExpandString(nil, r.Replace, name, m)); n != "" {
				name, rewritten = n, true
			}
			break
		}
	}
	add("container_name", name)
	if !rewritten {
		add("container_id", dockerapi.ShortID(id))
	}
	img, tag := dockerapi.ParseImage(image)
	add("image_name", img)
	add("image_tag", tag)
	project, svc := labels[LabelComposeProject], labels[LabelComposeService]
	add("compose_project", project)
	add("compose_service", svc)
	switch {
	case labels[LabelService] != "":
		add("service", labels[LabelService])
	case project != "" && svc != "":
		add("service", project+"-"+svc)
	}
	return out
}

// Tags returns the tags the collector puts on a container's metrics, for
// other components that report about a container (autodiscovery tags a
// check it started for one the same way, so its metrics join the
// container's on a dashboard).
func Tags(c dockerapi.Container, rewrites []Rewrite) []string {
	return tagger{rewrites: rewrites}.tags(c.Name(), c.ID, c.Image, c.Labels)
}
