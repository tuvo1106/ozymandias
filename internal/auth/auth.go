package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// SessionCookie is the admin session's cookie name.
const SessionCookie = "ozy_session"

// Options configures an Auth.
type Options struct {
	Keys          *Keyring
	AdminPassword string        // required: an empty one would be a door with no lock
	SessionTTL    time.Duration // 0: 12h
	Clock         clock.Clock
	Registry      *selfmetrics.Registry
	// Secure sets the cookie's Secure flag: true behind TLS.
	Secure bool
	// LoginDelay is how long a wrong password holds the login gate. 0: 300ms.
	LoginDelay time.Duration
}

// Auth is the gate in front of ozyd.
type Auth struct {
	opts  Options
	admin [sha256.Size]byte
	rej   *Rejections
	gate  chan struct{} // one wrong-password guess at a time
	queue chan struct{} // how many may wait for the gate

	mu       sync.Mutex
	sessions map[string]time.Time
}

// maxSessions bounds the session table. Logins are throttled, so reaching it
// means something is wrong; the oldest go first.
const maxSessions = 1000

// loginWaiters is how many wrong guesses may queue behind the gate before the
// rest are refused outright.
const loginWaiters = 8

// New builds an Auth. It returns an error rather than a server with no password.
func New(o Options) (*Auth, error) {
	if o.AdminPassword == "" {
		return nil, errors.New("auth: the admin password is empty")
	}
	if o.Keys == nil {
		return nil, errors.New("auth: no keyring")
	}
	if o.SessionTTL == 0 {
		o.SessionTTL = 12 * time.Hour
	}
	if o.LoginDelay == 0 {
		o.LoginDelay = 300 * time.Millisecond
	}
	o.Keys.SetRejections(NewRejections(o.Registry))
	a := &Auth{
		opts:     o,
		admin:    sha256.Sum256([]byte(o.AdminPassword)),
		rej:      o.Keys.rej,
		gate:     make(chan struct{}, 1),
		queue:    make(chan struct{}, loginWaiters),
		sessions: map[string]time.Time{},
	}
	return a, nil
}

// Rejected returns the rejection counters, so a Principal's limits can share them.
func (a *Auth) Rejected() *Rejections { return a.rej }

type ctxKey struct{}

// sizeKey carries what the gate charged for the request's bytes, for a refund.
type sizeKey struct{}

// FromContext returns the key that authenticated the request, or nil (auth off,
// or a path that takes no key). Every Principal method is safe on nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

type route int

const (
	public route = iota
	ingest
	admin
)

// classify decides what a request must present, by what the path *is*.
// Anything not known to be public or ingest is admin: a new endpoint is
// protected until someone says otherwise.
func classify(r *http.Request) route {
	p := r.URL.Path
	switch {
	case p == "/healthz" || p == "/readyz":
		return public
	case p == "/api/v1/auth/login" || p == "/api/v1/auth/logout" || p == "/api/v1/auth/session":
		return public
	case strings.HasPrefix(p, "/v1/"):
		return ingest
	case strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/debug/"):
		return admin
	}
	return public // the UI's static files: the page that shows the login form
}

// Wrap puts the gate in front of next.
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch classify(r) {
		case ingest:
			a.serveIngest(w, r, next)
		case admin:
			switch a.adminStatus(r) {
			case http.StatusOK:
			case http.StatusTooManyRequests:
				r.Pattern = "auth-reject"
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "too many password attempts")
				return
			default:
				a.refuse(w, r, http.StatusUnauthorized, "login required")
				return
			}
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (a *Auth) serveIngest(w http.ResponseWriter, r *http.Request, next http.Handler) {
	p, ok := a.opts.Keys.Authenticate(r.Header.Get(wire.HeaderKey))
	if !ok {
		a.rej.Inc(ReasonUnauthorized)
		a.refuse(w, r, http.StatusUnauthorized, "missing or invalid "+wire.HeaderKey)
		return
	}
	size := r.ContentLength
	if size < 0 || size > wire.MaxBodyBytes {
		size = wire.MaxBodyBytes // unknown length is charged as the largest body
	}
	if ok, reason, retry := p.Allow(size); !ok {
		a.rej.Inc(reason)
		r.Pattern = "auth-limit"
		Refuse(w, retry)
		return
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, p)
	r2 := r.WithContext(context.WithValue(ctx, sizeKey{}, size))
	next.ServeHTTP(w, r2)
	// The mux names the route on the copy it was given; the request counter
	// reads the original, and would file every authenticated request as
	// "unmatched".
	r.Pattern = r2.Pattern
}

// refuse answers a rejected request. The route is named so that the
// ozy.http.requests series is not "unmatched" (the mux never saw it).
func (a *Auth) refuse(w http.ResponseWriter, r *http.Request, code int, msg string) {
	r.Pattern = "auth-reject"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Status: "error", Error: msg})
}

// Refuse writes a 429 saying when to come back. Retry-After is whole seconds,
// rounded up: rounding down would invite a retry that is refused again.
func Refuse(w http.ResponseWriter, retry time.Duration) {
	secs := max(1, int(math.Ceil(retry.Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Status: "error", Error: "rate limit exceeded"})
}

// hasSession reports whether the request carries a live session cookie.
func (a *Auth) hasSession(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	return err == nil && a.validSession(c.Value)
}

// adminStatus decides an admin request: 200 for a live session cookie or the
// right password as a bearer token (for scripts with no cookie jar), 429 when
// too many password guesses are already in flight, 401 otherwise. A bearer
// password is a guess like any other and goes through the same gate as the login
// form; if it did not, the login's throttling would guard nothing, since any
// admin route would answer guesses at full speed.
func (a *Auth) adminStatus(r *http.Request) int {
	if a.hasSession(r) {
		return http.StatusOK
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return http.StatusUnauthorized
	}
	switch a.tryPassword(r.Context(), strings.TrimPrefix(h, "Bearer ")) {
	case guessRight:
		return http.StatusOK
	case guessBusy:
		return http.StatusTooManyRequests
	}
	return http.StatusUnauthorized
}

type guess int

const (
	guessRight guess = iota
	guessWrong
	guessBusy
)

// tryPassword checks one password guess. Wrong guesses go through a gate that
// admits one at a time and holds each for LoginDelay, with at most loginWaiters
// waiting: guessing is bounded to a few per second whatever the attacker does
// in parallel, and a correct password does not wait for the delay.
//
// The cost of that bound is that a sustained flood of wrong guesses keeps the
// waiting places full, and a correct login then answers busy (429) until the
// flood eases. A session already held keeps working, since it never reaches
// the gate. A per-address limit would avoid that, but behind a proxy every
// client has the proxy's address.
func (a *Auth) tryPassword(ctx context.Context, pw string) guess {
	select {
	case a.queue <- struct{}{}:
	default:
		return guessBusy
	}
	defer func() { <-a.queue }()
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return guessBusy
	}
	if a.passwordOK(pw) {
		<-a.gate
		return guessRight
	}
	t := time.NewTimer(a.opts.LoginDelay)
	select {
	case <-t.C:
	case <-ctx.Done():
	}
	t.Stop()
	<-a.gate
	return guessWrong
}

func (a *Auth) passwordOK(pw string) bool {
	sum := sha256.Sum256([]byte(pw)) // equal lengths, so the compare is constant-time
	return subtle.ConstantTimeCompare(sum[:], a.admin[:]) == 1
}

func (a *Auth) validSession(tok string) bool {
	if tok == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[tok]
	if !ok {
		return false
	}
	if !a.opts.Clock.Now().Before(exp) {
		delete(a.sessions, tok)
		return false
	}
	return true
}

func (a *Auth) newSession() (string, time.Time, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	now := a.opts.Clock.Now()
	exp := now.Add(a.opts.SessionTTL)
	a.mu.Lock()
	defer a.mu.Unlock()
	for t, e := range a.sessions {
		if !now.Before(e) {
			delete(a.sessions, t)
		}
	}
	for len(a.sessions) >= maxSessions {
		var oldest string
		var oe time.Time
		for t, e := range a.sessions {
			if oldest == "" || e.Before(oe) {
				oldest, oe = t, e
			}
		}
		delete(a.sessions, oldest)
	}
	a.sessions[tok] = exp
	return tok, exp, nil
}

// Register mounts login, logout, session status and the key API.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("POST /api/v1/auth/logout", a.logout)
	mux.HandleFunc("GET /api/v1/auth/session", func(w http.ResponseWriter, r *http.Request) {
		// Cookie only: answering for a bearer password would make this public
		// endpoint a free oracle for guesses.
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "authenticated": a.hasSession(r)})
	})
	(&keyAPI{keys: a.opts.Keys}).register(mux)
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected {\"password\": ...}")
		return
	}
	switch a.tryPassword(r.Context(), body.Password) {
	case guessBusy:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	case guessRight:
		tok, exp, err := a.newSession()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not start a session")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: SessionCookie, Value: tok, Path: "/", Expires: exp, HttpOnly: true,
			Secure: a.opts.Secure, SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}
	writeError(w, http.StatusUnauthorized, "wrong password")
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, wire.ErrorResponse{Status: "error", Error: msg})
}
