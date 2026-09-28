package metricql

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateVocabulary = flag.Bool("update-vocabulary", false,
	"rewrite web/src/lib/metricqlVocabulary.json from Words()")

// webVocabulary is the UI's copy. The test reaches across the tree on
// purpose: the point is that the two cannot disagree, and the only place
// that can see both is a test that reads one and computes the other.
var webVocabulary = filepath.Join("..", "..", "..", "web", "src", "lib", "metricqlVocabulary.json")

func TestVocabulary_WebCopyIsCurrent(t *testing.T) {
	want, err := json.MarshalIndent(Words(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *updateVocabulary {
		if err := os.WriteFile(webVocabulary, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(webVocabulary)
	if err != nil {
		t.Fatalf("%v — run: go test ./internal/query/metricql -run Vocabulary -update-vocabulary", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run: go test ./internal/query/metricql -run Vocabulary -update-vocabulary\nwant:\n%s", webVocabulary, want)
	}
}

// Every word the editor offers has to be one the parser takes in that
// position, or the editor's completion is a parse error waiting to happen.
func TestVocabulary_EveryWordParses(t *testing.T) {
	w := Words()
	var queries []string
	for _, a := range w.Aggregators {
		queries = append(queries, a+":m{*}")
	}
	for _, f := range w.Functions {
		sig, _ := Lookup(f)
		args := ""
		for i := 0; i < sig.Required; i++ {
			if i > 0 {
				args += ", "
			}
			switch sig.Args[i] {
			case ArgNumber:
				args += "0.5"
			default:
				args += "sum:m{*}"
			}
		}
		queries = append(queries, f+"("+args+")")
	}
	lists := map[string][]string{"rollup_methods": w.RollupMethods, "fill_modes": w.FillModes}
	for _, m := range w.Modifiers {
		if m.Argument == "" {
			queries = append(queries, "sum:m{*}."+m.Name+"()")
			continue
		}
		values, ok := lists[m.Argument]
		if !ok || len(values) == 0 {
			t.Fatalf("modifier %s names argument list %q, which the vocabulary does not have", m.Name, m.Argument)
		}
		for _, v := range values {
			queries = append(queries, "sum:m{*}."+m.Name+"("+v+")")
		}
	}
	for _, q := range queries {
		if _, err := Parse(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// The modifier table is the one list not read straight off a parser table,
// so it is the one that could fall behind the switch in [parser.modifier].
// The switch's refusal names every modifier it accepts, so the two sets are
// compared in both directions: a name in the table the parser refuses is
// caught above, and a name the parser accepts that the table lacks — which
// the editor would then never offer — is caught here.
func TestVocabulary_ModifiersMatchTheParser(t *testing.T) {
	_, err := Parse("sum:m{*}.nosuch()")
	if err == nil {
		t.Fatal("an unknown modifier parsed")
	}
	msg := err.Error()
	i := strings.Index(msg, "(want ")
	j := strings.LastIndex(msg, ")")
	if i < 0 || j < i {
		t.Fatalf("cannot find the modifier list in %q", msg)
	}
	named := strings.FieldsFunc(msg[i+len("(want "):j], func(r rune) bool { return r == ',' || r == ' ' })
	var parser []string
	for _, w := range named {
		if w != "or" {
			parser = append(parser, w)
		}
	}
	var table []string
	for _, m := range modifierWords {
		table = append(table, m.Name)
	}
	slices.Sort(parser)
	slices.Sort(table)
	if !slices.Equal(parser, table) {
		t.Fatalf("parser accepts %v, vocabulary offers %v", parser, table)
	}
}
