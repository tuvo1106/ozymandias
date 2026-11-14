package auth

import "github.com/tuvo1106/ozymandias/internal/selfmetrics"

// Rejections counts refused requests by reason. Every reason's counter exists
// from the start, so a chart of "rejected" is a flat zero line and not a blank
// one until the first rejection.
type Rejections struct {
	c map[string]*selfmetrics.Counter
}

// NewRejections registers ozy.intake.rejected{reason} for every reason.
// A nil registry yields counters that go nowhere.
func NewRejections(reg *selfmetrics.Registry) *Rejections {
	r := &Rejections{c: make(map[string]*selfmetrics.Counter, len(Reasons))}
	if reg == nil {
		return r
	}
	for _, reason := range Reasons {
		r.c[reason] = reg.Counter("ozy.intake.rejected", "reason:"+reason)
	}
	return r
}

// Inc counts one rejection. A reason not in Reasons counts nothing: the set of
// tag values is fixed in code.
func (r *Rejections) Inc(reason string) {
	if r == nil {
		return
	}
	if c := r.c[reason]; c != nil {
		c.Inc()
	}
}
