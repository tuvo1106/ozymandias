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

// Rewrite renames containers whose name matches Match to Replace (Go regexp
// replacement syntax, so $1 works), before the name becomes a tag.
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
		if r.Match.MatchString(name) {
			name, rewritten = r.Match.ReplaceAllString(name, r.Replace), true
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
