package logql

import "testing"

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(`service:web-api status:error "timeout" @route:/orders* @ms:>200 -@user:bot`); err != nil {
			b.Fatal(err)
		}
	}
}

// The scan's inner loop: one compiled filter over many logs.
func BenchmarkMatch(b *testing.B) {
	n, _ := Parse(`"connection refused" @ms:>200 -@user:bot`)
	f := Compile(n)
	hit := logFrom(b, "upstream: Connection refused by 10.0.0.4", "error", `{"ms":340,"user":"bob","route":"/orders"}`)
	miss := logFrom(b, "GET /orders 200 handled in time", "info", `{"ms":12,"user":"bob","route":"/orders"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if f(hit) == f(miss) {
			b.Fatal("filter does not discriminate")
		}
	}
}
