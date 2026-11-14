package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

type env struct {
	a    *Auth
	keys *Keyring
	clk  *testutil.FakeClock
	reg  *selfmetrics.Registry
	h    http.Handler
	seen *Principal // what the wrapped handler saw
}

func newEnv(t *testing.T, defaults Limits) *env {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := testutil.NewFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	keys, err := NewKeyring(context.Background(), db, clk, defaults)
	if err != nil {
		t.Fatal(err)
	}
	reg := selfmetrics.NewRegistry()
	a, err := New(Options{Keys: keys, AdminPassword: "hunter2", Clock: clk, Registry: reg, LoginDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{a: a, keys: keys, clk: clk, reg: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/series", func(w http.ResponseWriter, r *http.Request) {
		e.seen = FromContext(r.Context())
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /api/v1/things", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	a.Register(mux)
	e.h = a.Wrap(mux)
	return e
}

func (e *env) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) counter(reason string) int {
	for _, p := range e.reg.Snapshot() {
		if p.Name == "ozy.intake.rejected" {
			for _, tg := range p.Tags {
				if tg == "reason:"+reason {
					return int(p.Value)
				}
			}
		}
	}
	return -1
}

func (e *env) newKey(t *testing.T, l Limits) string {
	t.Helper()
	k, _, err := e.keys.Create(context.Background(), "test", l)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestIngest_NeedsAValidKey(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{})
	for name, hdr := range map[string]map[string]string{
		"none":      nil,
		"garbage":   {wire.HeaderKey: "nope"},
		"prefix":    {wire.HeaderKey: "ozy_"},
		"wrong":     {wire.HeaderKey: "ozy_" + strings.Repeat("a", 52)},
		"truncated": {wire.HeaderKey: key[:len(key)-1]},
		"as bearer": {"Authorization": "Bearer " + key},
	} {
		if w := e.do("POST", "/v1/series", "{}", hdr); w.Code != 401 {
			t.Errorf("%s: status %d, want 401", name, w.Code)
		}
	}
	if w := e.do("POST", "/v1/series", "{}", map[string]string{wire.HeaderKey: key}); w.Code != 202 {
		t.Fatalf("valid key: %d", w.Code)
	}
	if e.seen == nil || e.seen.Name != "test" {
		t.Fatalf("handler did not see the principal: %+v", e.seen)
	}
	if got := e.counter(ReasonUnauthorized); got != 6 {
		t.Fatalf("unauthorized counter = %d, want 6", got)
	}
}

func TestIngest_ARevokedKeyStopsAtOnce(t *testing.T) {
	e := newEnv(t, Limits{})
	_, row, _ := e.keys.Create(context.Background(), "x", Limits{})
	_ = row
	key := e.newKey(t, Limits{})
	rows, _ := e.keys.List(context.Background())
	id := rows[len(rows)-1].ID
	hdr := map[string]string{wire.HeaderKey: key}
	if w := e.do("POST", "/v1/series", "{}", hdr); w.Code != 202 {
		t.Fatalf("before revoke: %d", w.Code)
	}
	if err := e.keys.Revoke(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/v1/series", "{}", hdr); w.Code != 401 {
		t.Fatalf("after revoke: %d, want 401", w.Code)
	}
}

func TestKeys_SurviveARestartAndStayRevoked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, _ := meta.Open(path)
	clk := testutil.NewFakeClock(time.Now())
	k1, _ := NewKeyring(context.Background(), db, clk, Limits{})
	live, _, _ := k1.Create(context.Background(), "live", Limits{})
	dead, row, _ := k1.Create(context.Background(), "dead", Limits{})
	_ = k1.Revoke(context.Background(), row.ID)
	_ = db.Close()
	db, err := meta.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	k2, _ := NewKeyring(context.Background(), db, clk, Limits{})
	if _, ok := k2.Authenticate(live); !ok {
		t.Error("a live key stopped working after a restart")
	}
	if _, ok := k2.Authenticate(dead); ok {
		t.Error("a revoked key works again after a restart")
	}
}

func TestKeys_OnlyTheHashIsStored(t *testing.T) {
	e := newEnv(t, Limits{})
	secret := e.newKey(t, Limits{})
	rows, _ := e.keys.List(context.Background())
	for _, r := range rows {
		if strings.Contains(r.SHA256, secret) || r.SHA256 == secret || len(r.SHA256) != 64 {
			t.Fatalf("stored value %q is not a sha256 of the key", r.SHA256)
		}
	}
	if !strings.HasPrefix(secret, "ozy_") {
		t.Fatalf("key %q has no prefix", secret)
	}
}

func TestIngest_RateLimitsAnswer429WithRetryAfter(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{RequestsPerSec: 2})
	hdr := map[string]string{wire.HeaderKey: key}
	codes := []int{}
	for range 4 {
		codes = append(codes, e.do("POST", "/v1/series", "{}", hdr).Code)
	}
	if codes[0] != 202 || codes[1] != 202 || codes[2] != 429 {
		t.Fatalf("codes = %v, want 2 admitted then refused", codes)
	}
	w := e.do("POST", "/v1/series", "{}", hdr)
	if ra := w.Header().Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want 1", ra)
	}
	e.clk.Advance(time.Second)
	if w := e.do("POST", "/v1/series", "{}", hdr); w.Code != 202 {
		t.Fatalf("after a second: %d, want 202", w.Code)
	}
	if got := e.counter(ReasonRequests); got < 2 {
		t.Fatalf("requests counter = %d", got)
	}
}

func TestIngest_ByteLimitUsesTheDeclaredLength(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{BytesPerSec: 1000})
	hdr := map[string]string{wire.HeaderKey: key}
	big := strings.Repeat("x", 600)
	if w := e.do("POST", "/v1/series", big, hdr); w.Code != 202 {
		t.Fatalf("first: %d", w.Code)
	}
	// The bucket holds a burst of at least one max body, so drain it first.
	n := 0
	for e.do("POST", "/v1/series", big, hdr).Code == 202 && n < 100000 {
		n++
	}
	if w := e.do("POST", "/v1/series", big, hdr); w.Code != 429 {
		t.Fatalf("an exhausted byte budget answered %d", w.Code)
	}
	if e.counter(ReasonBytes) < 1 {
		t.Fatal("bytes rejection not counted")
	}
}

func TestIngest_DefaultsApplyToKeysWithoutLimits(t *testing.T) {
	e := newEnv(t, Limits{RequestsPerSec: 1})
	own := e.newKey(t, Limits{})
	hdr := map[string]string{wire.HeaderKey: own}
	e.do("POST", "/v1/series", "{}", hdr)
	if w := e.do("POST", "/v1/series", "{}", hdr); w.Code != 429 {
		t.Fatalf("the default of 1/s was not applied: %d", w.Code)
	}
}

func TestSpansAndLogBytes(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{SpansPerSec: 10, LogBytesPerDay: 1000})
	p, _ := e.keys.Authenticate(key)
	if ok, _ := p.AllowSpans(5000); !ok { // the burst is at least a full batch
		t.Fatal("a first batch was refused")
	}
	ok, retry := p.AllowSpans(5000)
	if ok || retry <= 0 {
		t.Fatalf("spans over the limit: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := p.AllowLogBytes(900); !ok {
		t.Fatal("under the day's allowance")
	}
	ok, retry = p.AllowLogBytes(200)
	if ok {
		t.Fatal("over the day's allowance was admitted")
	}
	if want := 12 * time.Hour; retry != want {
		t.Fatalf("retry = %v, want the time to midnight UTC (%v)", retry, want)
	}
	e.clk.Advance(13 * time.Hour)
	if ok, _ := p.AllowLogBytes(200); !ok {
		t.Fatal("the allowance did not reset at midnight")
	}
	var nilP *Principal
	if ok, _ := nilP.AllowSpans(1 << 30); !ok {
		t.Fatal("a nil principal (auth off) must admit")
	}
	if e.counter(ReasonSpans) != 1 || e.counter(ReasonLogBytesDay) != 1 {
		t.Fatalf("counters: spans=%d logs=%d", e.counter(ReasonSpans), e.counter(ReasonLogBytesDay))
	}
}

func TestDayCounter_OneOversizedRequestIsLetThroughOnAnEmptyDay(t *testing.T) {
	c := newDayCounter(100)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if ok, _ := c.add(500, now); !ok {
		t.Fatal("a request bigger than the whole allowance wedged the sender for ever")
	}
	if ok, _ := c.add(1, now); ok {
		t.Fatal("but nothing more is admitted that day")
	}
}

func TestAdmin_RoutesAreClosedByDefault(t *testing.T) {
	e := newEnv(t, Limits{})
	for _, path := range []string{"/api/v1/things", "/api/v1/keys", "/debug/vars", "/api/v1/anything/new"} {
		if w := e.do("GET", path, "", nil); w.Code != 401 {
			t.Errorf("GET %s without a login: %d, want 401", path, w.Code)
		}
	}
	for _, path := range []string{"/healthz", "/", "/assets/app.js", "/api/v1/auth/session"} {
		if w := e.do("GET", path, "", nil); w.Code == 401 {
			t.Errorf("GET %s should be reachable without a login", path)
		}
	}
	// An ingest key is not an admin credential.
	key := e.newKey(t, Limits{})
	if w := e.do("GET", "/api/v1/things", "", map[string]string{wire.HeaderKey: key}); w.Code != 401 {
		t.Errorf("an ingest key opened the admin API: %d", w.Code)
	}
}

func TestLogin_SessionCookieAndBearer(t *testing.T) {
	e := newEnv(t, Limits{})
	if w := e.do("POST", "/api/v1/auth/login", `{"password":"wrong"}`, nil); w.Code != 401 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w := e.do("POST", "/api/v1/auth/login", `{"password":"hunter2"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var c *http.Cookie
	for _, x := range w.Result().Cookies() {
		if x.Name == SessionCookie {
			c = x
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie missing or weak: %+v", c)
	}
	r := httptest.NewRequest("GET", "/api/v1/things", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("with the cookie: %d", rec.Code)
	}
	if w := e.do("GET", "/api/v1/things", "", map[string]string{"Authorization": "Bearer hunter2"}); w.Code != 200 {
		t.Fatalf("with the password as bearer: %d", w.Code)
	}
	if w := e.do("GET", "/api/v1/things", "", map[string]string{"Authorization": "Bearer hunter3"}); w.Code != 401 {
		t.Fatalf("wrong bearer: %d", w.Code)
	}
	// Sessions expire.
	e.clk.Advance(13 * time.Hour)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("an expired session still works: %d", rec.Code)
	}
}

func TestLogin_LogoutEndsTheSession(t *testing.T) {
	e := newEnv(t, Limits{})
	w := e.do("POST", "/api/v1/auth/login", `{"password":"hunter2"}`, nil)
	c := w.Result().Cookies()[0]
	r := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	r.AddCookie(c)
	e.h.ServeHTTP(httptest.NewRecorder(), r)
	r = httptest.NewRequest("GET", "/api/v1/things", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("a logged-out session still works: %d", rec.Code)
	}
}

func TestLogin_WrongGuessesAreSerialisedAndBounded(t *testing.T) {
	e := newEnv(t, Limits{})
	e.a.opts.LoginDelay = 50 * time.Millisecond
	const n = 40
	codes := make(chan int, n)
	for range n {
		go func() { codes <- e.do("POST", "/api/v1/auth/login", `{"password":"x"}`, nil).Code }()
	}
	got := map[int]int{}
	for range n {
		got[<-codes]++
	}
	if got[429] == 0 {
		t.Fatalf("%d parallel wrong guesses were all compared: %v", n, got)
	}
	if got[401] > loginWaiters+2 {
		t.Fatalf("%d guesses were compared at once, the bound is %d: %v", got[401], loginWaiters, got)
	}
	// And the owner still gets in afterwards.
	if w := e.do("POST", "/api/v1/auth/login", `{"password":"hunter2"}`, nil); w.Code != 200 {
		t.Fatalf("the owner was locked out: %d", w.Code)
	}
}

func TestKeyAPI_CreateListRevoke(t *testing.T) {
	e := newEnv(t, Limits{})
	admin := map[string]string{"Authorization": "Bearer hunter2"}
	w := e.do("POST", "/api/v1/keys", `{"name":"billing-svc","limits":{"requests_per_sec":5}}`, admin)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"key":"ozy_`) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	w = e.do("GET", "/api/v1/keys", "", admin)
	if strings.Contains(w.Body.String(), "ozy_") || strings.Contains(w.Body.String(), "sha") {
		t.Fatalf("the listing leaks a secret or hash: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), "billing-svc") {
		t.Fatalf("listing: %s", w.Body)
	}
	if w := e.do("DELETE", "/api/v1/keys/1", "", admin); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/v1/keys/999", "", admin); w.Code != 404 {
		t.Fatalf("revoke unknown: %d", w.Code)
	}
	if w := e.do("POST", "/api/v1/keys", `{"name":""}`, admin); w.Code != 400 {
		t.Fatalf("nameless key: %d", w.Code)
	}
	if w := e.do("POST", "/api/v1/keys", `{"name":"x","limits":{"requests_per_sec":-1}}`, admin); w.Code != 400 {
		t.Fatalf("negative limit: %d", w.Code)
	}
}

func TestNew_RefusesNoPassword(t *testing.T) {
	e := newEnv(t, Limits{})
	if _, err := New(Options{Keys: e.keys, Clock: e.clk}); err == nil {
		t.Fatal("an Auth with no password was built")
	}
}

// A fixed set of tag values, whatever the client sends.
func TestRejections_TagValuesAreFixed(t *testing.T) {
	e := newEnv(t, Limits{})
	for i := range 200 {
		e.do("POST", "/v1/series", "{}", map[string]string{wire.HeaderKey: strings.Repeat("k", i)})
	}
	n := 0
	for _, p := range e.reg.Snapshot() {
		if p.Name == "ozy.intake.rejected" {
			n++
		}
	}
	if n != len(Reasons) {
		t.Fatalf("%d rejected series, want exactly %d", n, len(Reasons))
	}
}

func TestSessions_AreBounded(t *testing.T) {
	e := newEnv(t, Limits{})
	for range maxSessions + 50 {
		if _, _, err := e.a.newSession(); err != nil {
			t.Fatal(err)
		}
	}
	e.a.mu.Lock()
	n := len(e.a.sessions)
	e.a.mu.Unlock()
	if n > maxSessions {
		t.Fatalf("%d sessions held, bound is %d", n, maxSessions)
	}
}

func TestSessions_AnOldestOneIsEvictedNotTheNewest(t *testing.T) {
	e := newEnv(t, Limits{})
	var first string
	for i := range maxSessions {
		tok, _, _ := e.a.newSession()
		if i == 0 {
			first = tok
		}
		e.clk.Advance(time.Millisecond) // distinct expiries
	}
	newest, _, _ := e.a.newSession()
	if e.a.validSession(first) {
		t.Error("the oldest session survived eviction")
	}
	if !e.a.validSession(newest) {
		t.Error("the newest session was evicted")
	}
}

func TestIngest_ARefusedBodyDoesNotCostARequestToken(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{RequestsPerSec: 100, BytesPerSec: 10})
	p, _ := e.keys.Authenticate(key)
	// Drain the byte bucket; its capacity is one full body, far over 10 B/s.
	for ok, _, _ := p.Allow(wire.MaxBodyBytes); ok; ok, _, _ = p.Allow(wire.MaxBodyBytes) {
	}
	before := p.requests.tokens
	if ok, reason, _ := p.Allow(wire.MaxBodyBytes); ok || reason != ReasonBytes {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
	if p.requests.tokens < before {
		t.Fatalf("a request refused for its size still spent a request token: %v -> %v", before, p.requests.tokens)
	}
}

func TestBucket_AnOversizedTakeWaitsForAFullBucketNotForEver(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBucket(10, 10, now)
	if ok, _ := b.take(1000, now); !ok { // a full bucket admits a request bigger than its capacity, once
		t.Fatal("a request larger than the whole bucket could never be admitted")
	}
	ok, wait := b.take(1000, now)
	if ok || wait != time.Second {
		t.Fatalf("ok=%v wait=%v, want to wait one second for the bucket to refill", ok, wait)
	}
}

func TestKeys_LastUsedIsWrittenAtMostOncePerMinute(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{})
	used := func() time.Time {
		rows, _ := e.keys.List(context.Background())
		return rows[len(rows)-1].LastUsedAt
	}
	e.keys.Authenticate(key)
	first := used()
	if first.IsZero() {
		t.Fatal("last_used_at was never written")
	}
	e.clk.Advance(10 * time.Second)
	e.keys.Authenticate(key)
	if !used().Equal(first) {
		t.Fatal("last_used_at was rewritten inside the minute: a commit per request")
	}
	e.clk.Advance(2 * time.Minute)
	e.keys.Authenticate(key)
	if !used().After(first) {
		t.Fatal("last_used_at never advanced")
	}
}

func TestKeyAPI_ListShowsUsageAndRevocation(t *testing.T) {
	e := newEnv(t, Limits{})
	admin := map[string]string{"Authorization": "Bearer hunter2"}
	key := e.newKey(t, Limits{BytesPerSec: 7})
	e.keys.Authenticate(key)
	e.do("DELETE", "/api/v1/keys/1", "", admin)
	w := e.do("GET", "/api/v1/keys", "", admin)
	for _, want := range []string{`"last_used_at":"`, `"revoked_at":"`, `"bytes_per_sec":7`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("listing lacks %s: %s", want, w.Body)
		}
	}
	if w := e.do("DELETE", "/api/v1/keys/abc", "", admin); w.Code != 400 {
		t.Errorf("a non-numeric id: %d", w.Code)
	}
}

func TestLogin_RejectsAMalformedBody(t *testing.T) {
	e := newEnv(t, Limits{})
	if w := e.do("POST", "/api/v1/auth/login", `not json`, nil); w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
	w := e.do("GET", "/api/v1/auth/session", "", nil)
	if !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatalf("session status: %s", w.Body)
	}
}

func TestNewRejections_NilRegistryAndUnknownReasonCountNothing(t *testing.T) {
	r := NewRejections(nil)
	r.Inc(ReasonBytes)
	r.Inc("whatever a client sent")
	var nilR *Rejections
	nilR.Inc(ReasonBytes)
}

// A body with no declared length is charged as the largest body there can be: a
// sender that omits Content-Length (chunked) must not get its bytes for free.
func TestIngest_AnUndeclaredLengthIsChargedAsTheLargestBody(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{BytesPerSec: 1000})
	hdr := map[string]string{wire.HeaderKey: key}
	chunked := func() int {
		r := httptest.NewRequest("POST", "/v1/series", strings.NewReader("{}"))
		r.ContentLength = -1
		r.Header.Set(wire.HeaderKey, key)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w.Code
	}
	if c := chunked(); c != 202 {
		t.Fatalf("first chunked request: %d", c)
	}
	if c := chunked(); c != 429 {
		t.Fatalf("a second chunked request straight after: %d, want 429 (the first was charged a whole body)", c)
	}
	_ = hdr
}
