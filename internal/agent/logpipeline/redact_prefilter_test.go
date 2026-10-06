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
		plain.text[i].need = needNone
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
	need := func(s string) bool { return needKV.ok(lowerInto(nil, s)) }
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

// A secret word spelled with the two non-ASCII runes that fold to ASCII under
// the rules' (?i) must still be found: the filters read a lowercase that maps them.
func TestRedactPrefilter_FoldedNonASCIISpellingsStillRedact(t *testing.T) {
	r, err := NewRedactor(RedactConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"api_\u212Aey=hunter2 ünïcode", "pa\u017F\u017Fword=hunter2", "to\u212Aen: hunter2",
		"AUTHORIZATION: bearer abc", "日本語 password=hunter2 emoji 😀", "\u017Fig=x https://a.test/?\u017Fig=hunter2",
	} {
		got, changed := r.String(in)
		if !changed || strings.Contains(got, "hunter2") || strings.Contains(got, "abc") {
			t.Errorf("%q leaked past the prefilter: %q", in, got)
		}
	}
	// And plain non-ASCII text is not a reason to give up the filters: it is skipped, unchanged.
	if got, changed := r.String("日本語のメッセージ 😀 ünïcode"); changed || got != "日本語のメッセージ 😀 ünïcode" {
		t.Errorf("non-ASCII text without secrets changed: %q", got)
	}
}

// Arbitrary strings over an alphabet built from the rules' own vocabulary: every
// letter of every secret word in both cases, every member of the two fold orbits,
// the separators and the structural characters. Where the fragment list above
// tests realistic lines, this one reaches shapes nobody wrote down.
func TestRedactPrefilter_AgreesWithThePlainRulesOnArbitraryText(t *testing.T) {
	filtered, plain := unfiltered(t, RedactConfig{})
	alphabet := []rune("abcdeghikmnoprstuyKS" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ" + "\u212A\u017F" + "=:?&@.-_ \"'[]{}>,;/\\é日😀" + "0123456789")
	words := []string{"password", "token", "secret", "api_key", "key", "authorization", "eyJ", "@", "cookie", "phone"}
	rapid.Check(t, func(t *rapid.T) {
		var b strings.Builder
		for i := 0; i < rapid.IntRange(0, 12).Draw(t, "n"); i++ {
			if rapid.Bool().Draw(t, "word") {
				w := rapid.SampledFrom(words).Draw(t, "w")
				if rapid.Bool().Draw(t, "fold") {
					w = strings.NewReplacer("k", "\u212A", "s", "\u017F").Replace(w)
				}
				b.WriteString(w)
			} else {
				b.WriteRune(rapid.SampledFrom(alphabet).Draw(t, "r"))
			}
		}
		in := b.String()
		got, gc := filtered.String(in)
		want, wc := plain.String(in)
		if got != want || gc != wc {
			t.Fatalf("input %q\n filtered: %q (%v)\n plain:    %q (%v)", in, got, gc, want, wc)
		}
	})
}

func TestLowerInto(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "AbC-1": "abc-1", "\u212Aey": "key", "\u017Fig": "sig", "é日": "é日", "\xe2\x84": "\xe2\x84", "\xc5": "\xc5"} {
		if got := string(lowerInto(nil, in)); got != want {
			t.Errorf("lowerInto(%q) = %q, want %q", in, got, want)
		}
	}
}
