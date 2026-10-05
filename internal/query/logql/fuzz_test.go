package logql

import (
	"errors"
	"reflect"
	"testing"
)

// L4: Parse never panics; an error points inside the query; and whatever it
// accepts prints to text that parses back to the same tree.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`service:api status:error "timeout"`, `@ms:>200 -@tag:x`, `(a OR b) AND NOT c`,
		`@ms:[1 TO 2]`, `"a\"b"`, `-(((a)))`, `@x:*`, `service:"my api" OR host:h*`,
		`a (`, `@ms:>`, `"abc`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		n, err := Parse(q)
		if err != nil {
			var pe *Error
			if !errors.As(err, &pe) || pe.Col < 1 || pe.Col > len(q)+1 {
				t.Fatalf("Parse(%q) error %v does not point inside the query", q, err)
			}
			return
		}
		if n == nil {
			return
		}
		again, err := Parse(n.String())
		if err != nil {
			t.Fatalf("Parse(%q) printed %q, which does not parse: %v", q, n.String(), err)
		}
		if !reflect.DeepEqual(again, n) {
			t.Fatalf("Parse(%q) printed %q, which parses to a different tree", q, n.String())
		}
	})
}
