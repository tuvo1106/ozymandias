package logpipeline

import "testing"

func defaultRedactor(t testing.TB) *Redactor {
	t.Helper()
	r, err := NewRedactor(RedactConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRedactor_RedactsSecretsAndKeepsTheRestOfTheLine(t *testing.T) {
	r := defaultRedactor(t)
	for in, want := range map[string]string{
		// JWTs
		"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk ok": "token [REDACTED] ok",
		// Authorization headers, in a few shapes
		"Authorization: Bearer abc123.def":                "Authorization: [REDACTED]",
		`headers={"authorization": "Basic dXNlcjpwdw=="}`: `headers={"authorization": "[REDACTED]"}`,
		"authorization=token ghp_xxxx next":               "authorization=[REDACTED] next",
		"Proxy-Authorization: Basic abc":                  "Proxy-Authorization: [REDACTED]",
		// reset / verify links: only the secret parameter value
		"send https://app.test/reset?token=s3cr3t&lang=en now": "send https://app.test/reset?token=[REDACTED]&lang=en now",
		"GET /cb?code=abc123&state=xyz HTTP/1.1":               "GET /cb?code=[REDACTED]&state=xyz HTTP/1.1",
		"https://x.test/v?signature=ZZ&expires=9":              "https://x.test/v?signature=[REDACTED]&expires=9",
		"/p?api_key=K1&password=P2":                            "/p?api_key=[REDACTED]&password=[REDACTED]",
		// key=value forms
		"login password=hunter2 failed":                                     "login password=[REDACTED] failed",
		`{"password": "correct horse battery"}`:                             `{"password": "[REDACTED]"}`,
		`{'api_key': 'abc def'}`:                                            `{'api_key': '[REDACTED]'}`,
		`params={"order"=>{"customer_phone"=>"+1 555 123 4567", "qty"=>2}}`: `params={"order"=>{"customer_phone"=>"[REDACTED]", "qty"=>2}}`,
		"client_secret: s3  next":                                           "client_secret: [REDACTED]  next",
		"access_token=aaa refresh_token=bbb":                                "access_token=[REDACTED] refresh_token=[REDACTED]",
		"csrftoken=abc authtoken: def":                                      "csrftoken=[REDACTED] authtoken: [REDACTED]",
		"ssn=123-45-6789":                                                   "ssn=[REDACTED]",
		// emails
		"user jane.doe+tag@example.co.uk signed in": "user [REDACTED] signed in",
	} {
		if got, _ := r.String(in); got != want {
			t.Errorf("redact(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

// What matters as much as catching secrets: ordinary text must come through
// untouched, or the logs stop being readable and nobody trusts the rules.
func TestRedactor_LeavesOrdinaryTextAlone(t *testing.T) {
	r := defaultRedactor(t)
	for _, in := range []string{
		"user logged in", "token expired", "the token was refused", "password reset requested for user 42",
		"secretary: bob", "microphone: on", "keyboard: us", "monkey=1", "the key to success",
		"token_count=5 tokens_used=7", "phone call ended", "GET /api/v1/healthz HTTP/1.1 200",
		"cookie banner shown", "authorization pending", "ssn lookup", "code=200", "status code: 404",
		"Exception in ASGI application", "redis_version=7.4.11 mem_usage=1.61M clients_connected=2",
		"email sent to the queue", "a@b", "50% @ 3pm", "https://example.com/path?page=2&sort=asc",
		"2026-10-04T12:00:00Z INFO started", "eyJ not a jwt", "x.y.z",
	} {
		if got, changed := r.String(in); changed || got != in {
			t.Errorf("ordinary text changed: %q -> %q", in, got)
		}
	}
}

func TestRedactor_Attrs(t *testing.T) {
	r := defaultRedactor(t)
	attrs := map[string]any{
		"password":      "hunter2",
		"accessToken":   "abc",
		"APIKey":        "k",
		"csrftoken":     "c",
		"customerPhone": "+1 555",
		"auth": map[string]any{
			"token": "t", "user": "jane@example.com", "ok": true,
			"list": []any{"fine", "reach me at bob@example.com", map[string]any{"secret": 1}},
		},
		"token_count": 5,
		"tokens_used": "7",
		"microphone":  "on",
		"message":     "see https://x.test/r?token=abc",
		"n":           3.5,
		"plain":       "hello",
	}
	if !r.Attrs(attrs) {
		t.Fatal("reported no change")
	}
	for _, k := range []string{"password", "accessToken", "APIKey", "customerPhone", "csrftoken"} {
		if attrs[k] != Redacted {
			t.Errorf("%s = %v", k, attrs[k])
		}
	}
	auth := attrs["auth"].(map[string]any)
	if auth["token"] != Redacted || auth["user"] != Redacted || auth["ok"] != true {
		t.Errorf("auth = %v", auth)
	}
	list := auth["list"].([]any)
	if list[0] != "fine" || list[1] != "reach me at [REDACTED]" || list[2].(map[string]any)["secret"] != Redacted {
		t.Errorf("list = %v", list)
	}
	for k, want := range map[string]any{"token_count": 5, "tokens_used": "7", "microphone": "on", "plain": "hello", "n": 3.5} {
		if attrs[k] != want {
			t.Errorf("%s = %v, want it untouched (%v)", k, attrs[k], want)
		}
	}
	if attrs["message"] != "see https://x.test/r?token=[REDACTED]" {
		t.Errorf("message = %v", attrs["message"])
	}
	// Redacting what is already redacted is a no-op (so a pipeline can run twice).
	if r.Attrs(attrs) {
		t.Error("a second pass changed already-redacted attrs")
	}
}

func TestRedactor_Config(t *testing.T) {
	r, err := NewRedactor(RedactConfig{
		Disable: []string{"email"},
		Rules:   []RuleSpec{{Pattern: `\b\d{3}-\d{4}\b`}, {Pattern: `(order-)\d+`, Replace: "${1}N"}},
		Keys:    []string{"customerRef"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.String("mail bob@example.com call 555-1234 for order-98765"); got != "mail bob@example.com call [REDACTED] for order-N" {
		t.Errorf("got %q", got)
	}
	attrs := map[string]any{"customer_ref": "c-1", "other": "x"}
	r.Attrs(attrs)
	if attrs["customer_ref"] != Redacted || attrs["other"] != "x" {
		t.Errorf("attrs = %v", attrs)
	}
	if _, err := NewRedactor(RedactConfig{Rules: []RuleSpec{{Pattern: "("}}}, nil); err == nil {
		t.Error("a bad pattern was accepted")
	}
	off, _ := NewRedactor(RedactConfig{Disable: []string{"jwt", "authorization", "url-secret", "kv-secret", "email"}}, nil)
	for _, in := range []string{"password=x", `password="a b"`, `{'secret': 'a b'}`, "a@b.co", "Authorization: Bearer q", "?token=1"} {
		if got, ch := off.String(in); ch {
			t.Errorf("with every default off %q became %q", in, got)
		}
	}
}

func TestRedactor_CountsWhichRuleFired(t *testing.T) {
	fired := map[string]int{}
	r, _ := NewRedactor(RedactConfig{}, func(rule string) { fired[rule]++ })
	r.String("Authorization: Bearer x and password=y")
	r.Attrs(map[string]any{"token": "t"})
	if fired["authorization"] != 1 || fired["kv-secret"] != 1 || fired["key"] != 1 {
		t.Errorf("fired = %v", fired)
	}
}

func TestSnake(t *testing.T) {
	for in, want := range map[string]string{
		"accessToken": "access_token", "APIKey": "api_key", "apiKey": "api_key", "customerPhone": "customer_phone",
		"already_snake": "already_snake", "HTTPServer": "http_server", "a": "a", "": "", "X-Api-Key": "x-api-key", "id2Token": "id2_token",
	} {
		if got := snake(in); got != want {
			t.Errorf("snake(%q) = %q, want %q", in, got, want)
		}
	}
}
