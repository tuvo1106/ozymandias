package metricql

import "slices"

// Vocabulary is every fixed word a query can contain, grouped by the position
// it can appear in. It exists for the dashboard editor's autocomplete, which
// needs the same lists the parser accepts and must not keep its own: a second
// list in TypeScript is one that goes stale the day an aggregator is added,
// and an editor that stops offering a word the parser accepts looks exactly
// like one where the word does not exist.
//
// The web UI carries a copy as JSON (web/src/lib/metricqlVocabulary.json),
// generated from this function and checked by a test in this package, so the
// copy cannot drift without `go test` failing. A build-time copy rather than
// an endpoint because the UI is embedded in the same binary as this parser:
// the two are always the same version, and an endpoint would add a
// "not loaded yet" and a "failed to load" state to every completion for no
// information it does not already have.
type Vocabulary struct {
	// Aggregators, the word before a query's ':'.
	Aggregators []string `json:"aggregators"`
	// Functions, called as name(args).
	Functions []string `json:"functions"`
	// Modifiers, the `.name(…)` chain after a query.
	Modifiers []ModifierWord `json:"modifiers"`
	// RollupMethods, the first argument of .rollup().
	RollupMethods []string `json:"rollup_methods"`
	// FillModes, the first argument of .fill().
	FillModes []string `json:"fill_modes"`
}

// ModifierWord is one modifier, and what its first argument is.
type ModifierWord struct {
	Name string `json:"name"`
	// Argument names the list the first argument comes from —
	// "rollup_methods" or "fill_modes" — or is empty for a modifier that
	// takes none, so an editor knows whether to complete `name()` whole.
	Argument string `json:"argument"`
}

// modifierWords is every modifier [parser.modifier] accepts. The parser
// dispatches with a switch, so this table is the list form of it;
// TestVocabulary_EveryWordParses is what keeps the two in step.
var modifierWords = []ModifierWord{
	{Name: string(ModAsCount)},
	{Name: string(ModAsRate)},
	{Name: string(ModFill), Argument: "fill_modes"},
	{Name: string(ModRollup), Argument: "rollup_methods"},
}

// Words returns the vocabulary, every list sorted.
func Words() Vocabulary {
	aggNames := make([]string, 0, len(aggs))
	for a := range aggs {
		aggNames = append(aggNames, string(a))
	}
	slices.Sort(aggNames)
	sorted := func(s []string) []string {
		out := slices.Clone(s)
		slices.Sort(out)
		return out
	}
	return Vocabulary{
		Aggregators:   aggNames,
		Functions:     FunctionNames(),
		Modifiers:     slices.Clone(modifierWords),
		RollupMethods: sorted(rollupMethods),
		FillModes:     sorted(fillModes),
	}
}
