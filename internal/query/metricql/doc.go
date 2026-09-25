// Package metricql parses metricql, the tag-oriented query language the
// dashboards and the query API are written in. It turns text into an AST and
// prints an AST back to canonical text. It does not evaluate: that is
// internal/query/metricql/eval, which walks what this package produces.
//
// A query reads as a pipeline, left to right:
//
//	sum:http.request.count{service:api,!status:2*} by {route}.as_rate()
//	└┬┘ └────────┬───────┘ └──────────┬─────────┘    └──┬─┘  └───┬───┘
//	 │           │                    │                 │        └ modifiers
//	 │           │                    │                 └ group the series
//	 │           │                    └ select the series
//	 │           └ of this metric
//	 └ combine each group with this
//
// # The grammar is not lexically uniform, and that shapes the lexer
//
// The obvious design — a lexer that turns the whole string into tokens, then
// a parser over those tokens — cannot work here, because the same byte is an
// operator in one position and ordinary text in another. In
// `route:/api/items` the '/' is part of a tag value; three tokens later, in
// `a / b`, it is division. A tag key may contain '-' and '/'
// (pkg/wire.ValidTag), so `by {status-code}` is one key and `a-b` is
// subtraction. No amount of lookahead fixes this: the difference is not in
// the text, it is in where the parser is.
//
// So the lexer here is pull-based and takes direction from the parser. It is
// a cursor over the source with three entry points — expression mode, tag-key
// mode, and tag-value mode — and the parser picks one each time it asks for
// the next token, because at every point it knows which kind of thing it is
// about to read. This is the same answer Go's own scanner gives to a smaller
// version of the problem (whether '/' begins a comment), and it is why the
// two are written together rather than as separate passes.
//
// The lexer is restartable by construction: its whole state is the source and
// an offset, so asking for a token in the wrong mode is recoverable by
// rewinding to that token's start.
//
// # The AST carries no positions
//
// Nodes are plain data: no offsets, no source spans. Two queries that mean
// the same thing are [reflect.DeepEqual], which is what makes the round-trip
// property (parse(print(ast)) == ast) a real test rather than a comparison
// modulo fields nobody reads.
//
// Error columns do not need positions in the tree. The parser knows where it
// is when it fails, so a [*Error] carries the column and nothing has to
// remember it afterwards. An evaluator reporting a problem with a subquery
// names it by its printed text, which is more use to a reader than a column
// into a string they may not have typed.
//
// # Canonical text
//
// [Node.String] prints a query in one canonical spelling: no redundant
// parentheses, tag keys lower-cased, IN in upper case, a bare "*" filter for
// a query with no matchers. Printing then parsing is the identity on the AST,
// so the API can echo a normalized "query" field, a dashboard can be
// diffed against what the editor produced, and a fuzz target can compare a
// parse against a re-parse instead of against a hand-written expectation.
package metricql
