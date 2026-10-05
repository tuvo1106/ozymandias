package logpipeline

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// unfiltered is the same redactor with every prefilter removed: the reference
// the filtered one must agree with on every input.
func unfiltered(t testing.TB, cfg RedactConfig) (*Redactor, *Redactor) {
	t.Helper()
	filtered, err := NewRedactor(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := NewRedactor(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range plain.text {
		plain.text[i].need = nil
	}
	return filtered, plain
}

var fragments = []string{
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln", "EYJabcd.efghij.k", "Authorization: Bearer abc123", "AUTHORIZATION=Basic xyz", "proxy-authorization: x",
	"https://x.test/cb?token=abc&lang=en", "?code=123", "&API_KEY=k1", "&apikey=k2", "?Access_Token=t", "?sig=zz",
	"password=hunter2", `"password": "pw"`, "PASSWD: x", "secret='s3'", "client_secret=>\"q\"", "api_key: k", "apiKey=k", "private-key=pk", "PRIVATE_KEY: pk",
	"credentials=c", "cookie: sid=1", "customer_phone=555", "phone: 1", "ssn=123-45-6789", "card_number=4111", "cardnumber: 4", "cvv=123", "CVC: 9",
	"access_token=t", "csrftoken: c", "authtoken=a", "token_count=3", "microphone=on", "secretary: bob", "x-api-key: abc", "db.password=x",
	"jane@example.test", "A.B+c@Sub.Example.CO", "name@", "@handle", "user@host", "no secrets here", "GET /orders 200", "key", "token", "", " ", "=", ":", "?", "&",
	"KEY=v", "Key: v", "ſecret=x", "Key=v", "tоken=x" /* cyrillic o */, "pássword=x", "テスト=token", "émail@example.test",
}

// The property that makes the prefilters safe: for any string, the filtered
// redactor returns exactly what the unfiltered one does. If a filter ever says
// "cannot match" for a string a rule would change, this fails and shows it.
func TestRedactPrefilter_NeverSkipsARuleThatWouldChangeTheString(t *testing.T) {
	filtered, plain := unfiltered(t, RedactConfig{})
	seps := []string{" ", "", "\n", "&", "?", "=", ":", `"`, ", "}
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(t, "n")
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(rapid.SampledFrom(fragments).Draw(t, "f"))
			b.WriteString(rapid.SampledFrom(seps).Draw(t, "s"))
		}
		in := b.String()
		if rapid.Bool().Draw(t, "upper") {
			in = strings.ToUpper(in)
		}
		got, gc := filtered.String(in)
		want, wc := plain.String(in)
		if got != want || gc != wc {
			t.Fatalf("input %q\n filtered: %q (%v)\n plain:    %q (%v)", in, got, gc, want, wc)
		}
	})
}

// Every key the kv rules can match contains one of the needles, whichever way
// the optional separator in the word lists is spelled.
func TestRedactPrefilter_CoversEverySecretWord(t *testing.T) {
	var words []string
	for _, group := range []string{wordsAnywhere, wordsAtEnd} {
		for _, w := range strings.Split(group, "|") {
			if strings.Contains(w, "[_-]?") {
				for _, sep := range []string{"", "_", "-"} {
					words = append(words, strings.Replace(w, "[_-]?", sep, 1))
				}
			} else {
				words = append(words, w)
			}
		}
	}
	if len(words) < 15 {
		t.Fatalf("only %d words expanded: the expansion is broken", len(words))
	}
	need := prefilter("kv-secret")
	for _, w := range words {
		if !need(w + "=x") {
			t.Errorf("the key %q would be matched by the kv rules but its needle check says no", w)
		}
	}
	for _, line := range []string{"nothing to see: here", "a=b c=d", "GET /x?y=1"} {
		if need(line) {
			t.Errorf("%q has no secret word but passes the kv prefilter", line)
		}
	}
}

func TestRedactPrefilter_NonASCIIStringsSkipTheFiltersAndStillRedact(t *testing.T) {
	r, err := NewRedactor(RedactConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A Kelvin sign (U+212A) folds to "k" under the rules' case-insensitive matching.
	got, changed := r.String("api_\u212Aey=hunter2 ünïcode")
	if !changed || strings.Contains(got, "hunter2") {
		t.Fatalf("a non-ASCII line leaked a secret past the prefilter: %q", got)
	}
}

func TestLowerASCII(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "AbC-1": "abc-1"} {
		if got, ok := lowerASCII(in); !ok || got != want {
			t.Errorf("lowerASCII(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := lowerASCII("é"); ok {
		t.Error("a non-ASCII string must not be trusted to the ASCII filters")
	}
}
