package docker

import "sync"

// DefaultMaxContainerNames is the cap the agent applies unless configured:
// well above any stack of named services (a compose project is tens), low
// enough that a daemon of generated names stops at a few hundred series per
// container metric instead of growing without bound.
const DefaultMaxContainerNames = 200

// OverflowName is the container_name a container gets when the agent has
// already admitted as many distinct names as [NameCap] allows.
const OverflowName = "other"

// NameCap bounds how many distinct container_name values one agent puts on
// its container metrics, folding every name past the limit into
// [OverflowName].
//
// Why it exists: a container name is a tag, a tag value is a series, and
// nothing about a Docker daemon bounds its names. Every unnamed `docker run`
// gets a fresh generated one (admiring_allen), so one machine running
// throwaway containers for a month mints a series per container, per metric,
// for as long as the TSDB retains them. container_name_rewrite is the
// precise remedy and stays the first choice, but it needs an operator to
// have predicted the pattern; this is the backstop for the names nobody did.
//
// Admission is first come, first kept, for the life of the process. A name,
// once admitted, is admitted for good, because a name that flipped to "other"
// mid-life would split one container's series in two. The price is that
// names which died long ago still hold their slots, so a long-running agent
// on a churning daemon fills up and then folds every new name; the fold
// counter (ozy.agent.docker.container_names_folded) is how an operator
// notices, and a restart empties the set. Evicting dead names instead was
// rejected: it would hand a slot to a new name whose series the TSDB keeps
// apart from the old one's only by tag, and make the cap bound nothing.
//
// The nil *NameCap and a limit of zero are both unlimited, so the cap is
// opt-out and every caller that has no cap needs no branch. Safe for
// concurrent use: the collector samples containers on several goroutines.
type NameCap struct {
	max    int
	onFold func()

	mu       sync.Mutex
	admitted map[string]struct{}
}

// NewNameCap returns a cap of max distinct names. onFold, if set, is called
// once per container the cap folds (not per name), so it counts what the
// cap is costing.
func NewNameCap(max int, onFold func()) *NameCap {
	return &NameCap{max: max, onFold: onFold, admitted: make(map[string]struct{})}
}

// Admit returns name if it is admitted, else [OverflowName].
func (c *NameCap) Admit(name string) string {
	if c == nil || c.max <= 0 {
		return name
	}
	c.mu.Lock()
	_, ok := c.admitted[name]
	if !ok && len(c.admitted) < c.max {
		c.admitted[name] = struct{}{}
		ok = true
	}
	c.mu.Unlock()
	if ok {
		return name
	}
	if c.onFold != nil {
		c.onFold()
	}
	return OverflowName
}
