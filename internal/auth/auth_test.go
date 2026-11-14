package auth

import (
	"context"
	"errors"
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
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if DeniedSpans(w, r, 5000) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		if DeniedLogBytes(w, r, 900) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /v1/fail", func(w http.ResponseWriter, r *http.Request) {
		if DeniedLogBytes(w, r, 900) || DeniedSpans(w, r, 4000) {
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable) // the store failed
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
	if w := e.do("POST", "/api/v1/keys", `{"name":"x","limits":{"requests_per_sec":-1}}`, admin); w.Code != 201 {
		t.Fatalf("a negative limit means unlimited and must be accepted: %d", w.Code)
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
	// The write happens off the request path, so wait for it.
	e.keys.Authenticate(key)
	testutil.Eventually(t, 2*time.Second, func() bool { return !used().IsZero() }, "last_used_at was never written")
	first := used()
	e.clk.Advance(10 * time.Second)
	e.keys.Authenticate(key)
	time.Sleep(100 * time.Millisecond) // a write that should not happen has had its chance
	if !used().Equal(first) {
		t.Fatal("last_used_at was rewritten inside the minute: a commit per request")
	}
	e.clk.Advance(2 * time.Minute)
	e.keys.Authenticate(key)
	testutil.Eventually(t, 2*time.Second, func() bool { return used().After(first) }, "last_used_at never advanced")
}

func TestKeyAPI_ListShowsUsageAndRevocation(t *testing.T) {
	e := newEnv(t, Limits{})
	admin := map[string]string{"Authorization": "Bearer hunter2"}
	key := e.newKey(t, Limits{BytesPerSec: 7})
	e.keys.Authenticate(key)
	testutil.Eventually(t, 2*time.Second, func() bool {
		rows, _ := e.keys.List(context.Background())
		return !rows[0].LastUsedAt.IsZero()
	}, "last_used_at was never written")
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

// A bearer password is a guess like any other: it goes through the login gate,
// and the public session probe never answers for it.
func TestAdmin_BearerGuessesAreBoundedAndTheProbeIsNoOracle(t *testing.T) {
	e := newEnv(t, Limits{})
	e.a.opts.LoginDelay = 50 * time.Millisecond
	const n = 40
	codes := make(chan int, n)
	for range n {
		go func() {
			codes <- e.do("GET", "/api/v1/things", "", map[string]string{"Authorization": "Bearer wrong"}).Code
		}()
	}
	got := map[int]int{}
	for range n {
		got[<-codes]++
	}
	if got[429] == 0 || got[401] > loginWaiters+2 {
		t.Fatalf("%d parallel wrong bearer guesses: %v, want them bounded like login's", n, got)
	}
	w := e.do("GET", "/api/v1/auth/session", "", map[string]string{"Authorization": "Bearer hunter2"})
	if !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatalf("the public probe answered for a bearer password: %s", w.Body)
	}
	if w := e.do("GET", "/api/v1/things", "", map[string]string{"Authorization": "Bearer hunter2"}); w.Code != 200 {
		t.Fatalf("the right bearer was refused: %d", w.Code)
	}
}

// What a flood of wrong guesses can and cannot do, said as a test so the claim in
// ADR-0059 is the one that holds: it can make a *new* login busy, and it cannot
// touch a session that already exists.
func TestLogin_AFloodMakesNewLoginsBusyButNotHeldSessions(t *testing.T) {
	e := newEnv(t, Limits{})
	w := e.do("POST", "/api/v1/auth/login", `{"password":"hunter2"}`, nil)
	c := w.Result().Cookies()[0]
	e.a.opts.LoginDelay = 300 * time.Millisecond
	stop := make(chan struct{})
	defer close(stop)
	for range loginWaiters + 4 {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					e.do("POST", "/api/v1/auth/login", `{"password":"x"}`, nil)
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	r := httptest.NewRequest("GET", "/api/v1/things", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("a held session was affected by the flood: %d", rec.Code)
	}
}

func TestLogout_NeedsNoSessionAndClearsTheCookie(t *testing.T) {
	e := newEnv(t, Limits{})
	r := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "stale"})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 204 {
		t.Fatalf("logout with a stale cookie: %d, want 204", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == SessionCookie && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("the stale cookie was not cleared")
	}
}

// A request refused for its spans or its log bytes gives back what the gate took,
// so the sender's retries run into that limit again, not into the request and byte
// limits (whose rejections would hide the cause).
func TestIngest_ASpansOrLogRefusalDoesNotDrainTheOtherBuckets(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{RequestsPerSec: 3, SpansPerSec: 1, LogBytesPerDay: 1000})
	hdr := map[string]string{wire.HeaderKey: key}
	if w := e.do("POST", "/v1/traces", "{}", hdr); w.Code != 202 {
		t.Fatalf("first: %d", w.Code)
	}
	for i := range 10 {
		e.clk.Advance(time.Second / 4) // some refill, never three requests' worth between tries
		w := e.do("POST", "/v1/traces", "{}", hdr)
		if w.Code != 429 {
			t.Fatalf("try %d: %d", i, w.Code)
		}
	}
	if got := e.counter(ReasonRequests); got != 0 {
		t.Fatalf("%d requests-limit refusals: spans refusals were charged as requests", got)
	}
	if got := e.counter(ReasonSpans); got != 10 {
		t.Fatalf("spans refusals = %d, want 10", got)
	}
	// Same for the day's log bytes.
	if w := e.do("POST", "/v1/logs", "{}", hdr); w.Code != 202 {
		t.Fatalf("first logs: %d", w.Code)
	}
	for i := range 10 {
		e.clk.Advance(time.Second / 4)
		if w := e.do("POST", "/v1/logs", "{}", hdr); w.Code != 429 {
			t.Fatalf("logs try %d: %d", i, w.Code)
		}
	}
	if got := e.counter(ReasonRequests); got != 0 {
		t.Fatalf("%d requests-limit refusals after log refusals", got)
	}
}

func TestLimits_ANegativeKeyLimitOptsOutOfADefault(t *testing.T) {
	e := newEnv(t, Limits{RequestsPerSec: 1})
	key := e.newKey(t, Limits{RequestsPerSec: -1})
	hdr := map[string]string{wire.HeaderKey: key}
	for i := range 50 {
		if w := e.do("POST", "/v1/series", "{}", hdr); w.Code != 202 {
			t.Fatalf("request %d: %d; the key asked for unlimited", i, w.Code)
		}
	}
}

func TestKeys_ALiveKeyIsFoundAmongManyRevokedOnes(t *testing.T) {
	e := newEnv(t, Limits{})
	for range 300 {
		_, row, _ := e.keys.Create(context.Background(), "old", Limits{})
		_ = e.keys.Revoke(context.Background(), row.ID)
	}
	live := e.newKey(t, Limits{})
	if _, ok := e.keys.Authenticate(live); !ok {
		t.Fatal("a live key was not found among revoked ones")
	}
	// The lookup is by hash, so its cost does not grow with the revoked keys; that
	// is by construction (a map), not something this test measures.
}

func TestIngest_ASpansRefusalGivesBackTheBytesToo(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{SpansPerSec: 1, BytesPerSec: 1000})
	hdr := map[string]string{wire.HeaderKey: key}
	body := strings.Repeat("x", 500)
	if w := e.do("POST", "/v1/traces", body, hdr); w.Code != 202 {
		t.Fatalf("first: %d", w.Code)
	}
	p, _ := e.keys.Authenticate(key)
	before := p.bytes.tokens
	if w := e.do("POST", "/v1/traces", body, hdr); w.Code != 429 {
		t.Fatalf("second: %d", w.Code)
	}
	if p.bytes.tokens < before {
		t.Fatalf("the refused request kept its 500 bytes: %v -> %v", before, p.bytes.tokens)
	}
}

// A request that ends in "try again" (a store failure, a full queue, a shed)
// stored nothing, so its retry must not be charged twice. Without this a store
// that is flapping turns the sender's 503s into its own request, byte, span and
// log-byte 429s.
func TestIngest_AFailedRequestGivesBackEverythingItSpent(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{RequestsPerSec: 2, BytesPerSec: 1000, SpansPerSec: 1, LogBytesPerDay: 1000})
	hdr := map[string]string{wire.HeaderKey: key}
	for i := range 20 {
		if w := e.do("POST", "/v1/fail", strings.Repeat("x", 500), hdr); w.Code != 503 {
			t.Fatalf("attempt %d: %d, want the handler's own 503 every time (it was charged again)", i, w.Code)
		}
	}
	for _, reason := range []string{ReasonRequests, ReasonBytes, ReasonSpans, ReasonLogBytesDay} {
		if got := e.counter(reason); got != 0 {
			t.Errorf("%s refusals = %d after 20 failed attempts", reason, got)
		}
	}
}

// But an answer that is final (a malformed body, say) keeps its charge: the
// sender will not retry it, and the work of reading it was done.
func TestIngest_AFinalRefusalKeepsItsCharge(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{RequestsPerSec: 2})
	e.h = e.a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	hdr := map[string]string{wire.HeaderKey: key}
	codes := []int{}
	for range 4 {
		codes = append(codes, e.do("POST", "/v1/series", "{}", hdr).Code)
	}
	if codes[0] != 400 || codes[1] != 400 || codes[2] != 429 {
		t.Fatalf("codes = %v: 400s must still cost a request", codes)
	}
}

func TestPrincipal_NilIsSafe(t *testing.T) {
	var p *Principal
	if ok, _, _ := p.Allow(1 << 30); !ok {
		t.Fatal("Allow on nil")
	}
	if ok, _ := p.AllowSpans(1); !ok {
		t.Fatal("AllowSpans on nil")
	}
	if ok, _ := p.AllowLogBytes(1); !ok {
		t.Fatal("AllowLogBytes on nil")
	}
	p.release(&charge{size: 1, spans: 1, logBytes: 1})
	p.release(nil)
}

// A closed keyring starts no more writes, and Close waits for those in flight, so
// the database can be closed after it.
func TestKeyring_CloseStopsAndWaitsForLastUsedWrites(t *testing.T) {
	e := newEnv(t, Limits{})
	key := e.newKey(t, Limits{})
	e.keys.Close()
	e.keys.Authenticate(key)
	time.Sleep(50 * time.Millisecond)
	rows, _ := e.keys.List(context.Background())
	if !rows[0].LastUsedAt.IsZero() {
		t.Fatal("a closed keyring still wrote last_used_at")
	}
}

type failingStore struct{ KeyStore }

func (failingStore) CreateAPIKey(context.Context, string, string, string, time.Time) (meta.APIKey, error) {
	return meta.APIKey{}, errors.New("meta: creating api key: database is locked")
}

// Only the caller's mistakes are 400s; our failures are 500s and do not repeat
// the database's wording.
func TestKeyAPI_AStoreFailureIs500AndGeneric(t *testing.T) {
	e := newEnv(t, Limits{})
	e.keys.store = failingStore{e.keys.store}
	w := e.do("POST", "/api/v1/keys", `{"name":"x"}`, map[string]string{"Authorization": "Bearer hunter2"})
	if w.Code != 500 || strings.Contains(w.Body.String(), "locked") || strings.Contains(w.Body.String(), "meta") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/v1/keys", `{"name":" "}`, map[string]string{"Authorization": "Bearer hunter2"}); w.Code != 400 {
		t.Fatalf("a blank name: %d", w.Code)
	}
}
