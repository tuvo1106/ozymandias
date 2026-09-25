package metricql_test

import (
	"errors"
	"fmt"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

func ExampleParse() {
	n, err := metricql.Parse("sum:http.request.count{service:api,!status:2*} by {route}.as_rate()")
	if err != nil {
		panic(err)
	}
	q := n.(*metricql.Query)
	fmt.Println("aggregator:", q.Agg)
	fmt.Println("metric:    ", q.Metric)
	for _, m := range q.Filter {
		fmt.Println("matcher:   ", m)
	}
	fmt.Println("by:        ", q.By)
	fmt.Println("modifiers: ", q.Modifiers)
	// Output:
	// aggregator: sum
	// metric:     http.request.count
	// matcher:    service:api
	// matcher:    !status:2*
	// by:         [route]
	// modifiers:  [as_rate()]
}

// A parse error carries the column to underline, which is what the query
// editor draws under the text as it is typed.
func ExampleError() {
	_, err := metricql.Parse("avg:http.request.duration{service:api by {route}")
	fmt.Println(err)

	var perr *metricql.Error
	if errors.As(err, &perr) {
		fmt.Println("underline column:", perr.Col)
	}
	// Output:
	// col 42: expected ',' or '}' but found '{'
	// underline column: 42
}

// Printing normalises: whatever spacing, capitalisation or redundant
// parentheses a query was written with, two queries that mean the same thing
// print the same. The API echoes this form, so a dashboard can be compared
// against what the editor produced without normalising it first.
func ExampleNode_String() {
	for _, src := range []string{
		"SUM : x { Env : prod } BY { Route }",
		"sum:x{env:prod} by {route}",
		"(sum:x{env:prod} by {route})",
	} {
		n, err := metricql.Parse(src)
		if err != nil {
			panic(err)
		}
		fmt.Println(n)
	}
	// Output:
	// sum:x{env:prod} by {route}
	// sum:x{env:prod} by {route}
	// sum:x{env:prod} by {route}
}

// A percentile aggregator is the planner's signal that a query has to be
// answered from the sketch store rather than from the series store.
func ExampleAgg_Quantile() {
	for _, src := range []string{"p95:d{*}", "avg:d{*}"} {
		q, err := metricql.Parse(src)
		if err != nil {
			panic(err)
		}
		agg := q.(*metricql.Query).Agg
		quantile, isPercentile := agg.Quantile()
		fmt.Printf("%s -> %v %v\n", agg, quantile, isPercentile)
	}
	// Output:
	// p95 -> 0.95 true
	// avg -> 0 false
}
