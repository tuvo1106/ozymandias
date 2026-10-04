package logql

import (
	"strconv"
	"strings"
)

// MinLiteral is the shortest literal a [Need] carries. The store's per-block
// bloom filter is built from three-byte windows of a block's text, so a literal
// shorter than that has nothing to look up and constrains nothing.
const MinLiteral = 3

// maxAlts bounds how many alternatives a Need may hold. Distributing AND over
// OR multiplies them, and a Need is only an optimization, so one that would
// grow past this is dropped in favour of a weaker (smaller) one.
const maxAlts = 16

// Need says what text a block of logs must contain for a query to possibly
// match any log in it. It is how a per-block bloom filter skips a block
// without decompressing it.
//
// Alts is a disjunction of conjunctions: the block can match only if, for some
// alternative, EVERY literal in it occurs (case-folded) in the block's text. A
// nil Alts means "no constraint": the query says nothing a filter can test, so
// the block cannot be skipped. An empty alternative means the same for its arm.
//
// The one property that matters is soundness: a log that matches the query
// always satisfies its block's Need. A Need may be weaker than it could be (a
// short literal, a numeric comparison, a NOT: all say nothing), never
// stronger. Soundness is checked against the matcher itself in the tests.
type Need struct{ Alts [][]string }

// None reports whether the Need constrains nothing.
func (n Need) None() bool { return len(n.Alts) == 0 }

// Needs derives the Need of a query's scan filter (the part of the query the
// index did not answer). Labels, NOT and numeric comparisons constrain
// nothing: a bloom filter holds text, and "absent" is not something it can
// prove.
func Needs(q Node) Need {
	switch q := q.(type) {
	case nil:
		return Need{}
	case Text:
		return literalNeed(strings.ToLower(q.Value))
	case Attr:
		if q.Op != OpEq {
			return Need{}
		}
		// Numbers compare as numbers, so @ms:1000 also finds 1e3, whose text
		// has no "1000" in it. A pattern that reads as a number says nothing
		// about text.
		if _, err := strconv.ParseFloat(q.Value, 64); err == nil {
			return Need{}
		}
		return literalNeed(strings.ToLower(q.Value))
	case And:
		return andNeeds(q.Args)
	case Or:
		return orNeeds(q.Args)
	}
	return Need{} // Label, Not
}

// literalNeed is the Need of one pattern: its literal runs between wildcards,
// those long enough to look up. All of them must occur, in one log, so in one block.
func literalNeed(lowerPattern string) Need {
	var lits []string
	for _, s := range splitWild(lowerPattern) {
		if len(s) >= MinLiteral {
			lits = append(lits, s)
		}
	}
	if len(lits) == 0 {
		return Need{}
	}
	return Need{Alts: [][]string{lits}}
}

// andNeeds: every conjunct's requirement must hold, so the alternatives are
// the cross product of the conjuncts'. A conjunct with no constraint adds
// nothing; one whose product would be too large is skipped, which only
// weakens the result.
func andNeeds(args []Node) Need {
	acc := [][]string{{}}
	for _, a := range args {
		n := Needs(a)
		if n.None() {
			continue
		}
		if len(acc)*len(n.Alts) > maxAlts {
			continue
		}
		var next [][]string
		for _, x := range acc {
			for _, y := range n.Alts {
				next = append(next, append(append([]string(nil), x...), y...))
			}
		}
		acc = next
	}
	if len(acc) == 1 && len(acc[0]) == 0 {
		return Need{}
	}
	return Need{Alts: acc}
}

// orNeeds: a log may match through any arm, so a block is skippable only if
// every arm is. One arm that constrains nothing makes the whole disjunction
// unconstrained.
func orNeeds(args []Node) Need {
	var alts [][]string
	for _, a := range args {
		n := Needs(a)
		if n.None() {
			return Need{}
		}
		alts = append(alts, n.Alts...)
	}
	if len(alts) > maxAlts {
		return Need{}
	}
	return Need{Alts: alts}
}

// OrNeeds combines the Needs of several branches a stream may satisfy: the
// block can match through any of them.
func OrNeeds(ns []Need) Need {
	var alts [][]string
	for _, n := range ns {
		if n.None() {
			return Need{}
		}
		alts = append(alts, n.Alts...)
	}
	if len(alts) > maxAlts {
		return Need{}
	}
	return Need{Alts: alts}
}
