package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// memOutbox is an in-memory Outbox.
type memOutbox struct {
	mu        sync.Mutex
	rows      []meta.OutboxRow
	events    []meta.EventRow
	failNextN int // CompleteOutbox fails this many times
}

func (m *memOutbox) Outbox(context.Context) ([]meta.OutboxRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]meta.OutboxRow(nil), m.rows...), nil
}

func (m *memOutbox) CompleteOutbox(_ context.Context, id int64, ev meta.EventRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextN > 0 {
		m.failNextN--
		return errors.New("disk full")
	}
	for i, r := range m.rows {
		if r.ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			break
		}
	}
	m.events = append(m.events, ev)
	return nil
}

func (m *memOutbox) add(id int64, notifier string) meta.OutboxRow {
	p, _ := json.Marshal(sample())
	r := meta.OutboxRow{ID: id, Created: t0, MonitorID: 7, GroupKey: "queue:default", Notifier: notifier, Payload: p}
	m.mu.Lock()
	m.rows = append(m.rows, r)
	m.mu.Unlock()
	return r
}

func (m *memOutbox) snapshot() ([]meta.OutboxRow, []meta.EventRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]meta.OutboxRow(nil), m.rows...), append([]meta.EventRow(nil), m.events...)
}

// scripted fails with errs in order, then succeeds.
type scripted struct {
	mu    sync.Mutex
	errs  []error
	calls int
	block bool // wait for ctx
}

func (s *scripted) Notify(ctx context.Context, _ Notification) error {
	s.mu.Lock()
	s.calls++
	i := s.calls - 1
	s.mu.Unlock()
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if i < len(s.errs) {
		return s.errs[i]
	}
	return nil
}

func (s *scripted) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type panicky struct{}

func (panicky) Notify(context.Context, Notification) error { panic("kaboom") }

func start(t *testing.T, d *Dispatcher) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("dispatcher did not stop")
		}
	}
}

func waitEvents(t *testing.T, ob *memOutbox, n int) []meta.EventRow {
	t.Helper()
	testutil.Eventually(t, 5*time.Second, func() bool {
		_, evs := ob.snapshot()
		return len(evs) >= n
	}, "want %d outcome events", n)
	_, evs := ob.snapshot()
	return evs
}

func TestDispatcherDeliversLeftoversAndRetries(t *testing.T) {
	testutil.CheckGoroutines(t)
	ob := &memOutbox{}
	ob.add(1, "flaky")
	ob.add(2, "dead")
	ob.add(3, "perm")
	ob.add(4, "ghost") // configured once, not any more
	ob.add(5, "panics")
	flaky := &scripted{errs: []error{errors.New("503"), errors.New("503")}}
	dead := &scripted{errs: []error{errors.New("a"), errors.New("b"), errors.New("c"), errors.New("d")}}
	perm := &scripted{errs: []error{&Permanent{errors.New("HTTP 400")}}}
	d := &Dispatcher{Notifiers: map[string]Notifier{"flaky": flaky, "dead": dead, "perm": perm, "panics": panicky{}},
		Store: ob, Backoff: []time.Duration{time.Millisecond, time.Millisecond}, Timeout: time.Second}
	stop := start(t, d)
	defer stop()
	evs := waitEvents(t, ob, 5)
	byText := map[string]meta.EventRow{}
	for _, e := range evs {
		byText[strings.SplitN(e.Text, ":", 2)[0]] = e
	}
	if e := byText["flaky"]; e.Kind != "notify_ok" || flaky.n() != 3 {
		t.Errorf("flaky: %+v after %d calls", e, flaky.n())
	}
	if e := byText["dead"]; e.Kind != "notify_failed" || dead.n() != 3 || !strings.Contains(e.Text, "after 3 attempts") {
		t.Errorf("dead: %+v after %d calls", e, dead.n())
	}
	if e := byText["perm"]; e.Kind != "notify_failed" || perm.n() != 1 {
		t.Errorf("permanent retried: %+v after %d calls", e, perm.n())
	}
	if e := byText["ghost"]; e.Kind != "notify_failed" || !strings.Contains(e.Text, `no notifier named "ghost"`) {
		t.Errorf("ghost: %+v", e)
	}
	if e := byText["panics"]; e.Kind != "notify_failed" || !strings.Contains(e.Text, "kaboom") {
		t.Errorf("panic: %+v", e)
	}
	if rows, _ := ob.snapshot(); len(rows) != 0 {
		t.Errorf("outbox not drained: %v", rows)
	}
	for _, e := range evs {
		if e.MonitorID != 7 || e.GroupKey != "queue:default" {
			t.Errorf("event not attributed: %+v", e)
		}
	}
}

// TestSlowNotifierDoesNotBlockOthers: each notifier has its own worker.
func TestSlowNotifierDoesNotBlockOthers(t *testing.T) {
	testutil.CheckGoroutines(t)
	ob := &memOutbox{}
	slow := &scripted{block: true}
	fast := &scripted{}
	d := &Dispatcher{Notifiers: map[string]Notifier{"slow": slow, "fast": fast}, Store: ob,
		Backoff: []time.Duration{}, Timeout: time.Hour}
	stop := start(t, d)
	testutil.Eventually(t, 5*time.Second, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.started }, "started")
	d.Enqueue(context.Background(), []meta.OutboxRow{ob.add(1, "slow"), ob.add(2, "fast"), ob.add(3, "fast")})
	evs := waitEvents(t, ob, 2)
	if fast.n() != 2 || evs[0].Kind != "notify_ok" || evs[1].Kind != "notify_ok" {
		t.Fatalf("fast notifier waited behind the slow one: %+v", evs)
	}
	stop()
	// Shutdown mid-delivery records no outcome; the row stays for next start.
	rows, evs := ob.snapshot()
	if len(rows) != 1 || rows[0].ID != 1 || len(evs) != 2 {
		t.Fatalf("after shutdown: rows %v events %v", rows, evs)
	}
}

func TestAttemptTimeout(t *testing.T) {
	testutil.CheckGoroutines(t)
	ob := &memOutbox{}
	ob.add(1, "hang")
	hang := &scripted{block: true}
	d := &Dispatcher{Notifiers: map[string]Notifier{"hang": hang}, Store: ob,
		Backoff: []time.Duration{time.Millisecond, time.Millisecond}, Timeout: 20 * time.Millisecond}
	stop := start(t, d)
	defer stop()
	evs := waitEvents(t, ob, 1)
	if evs[0].Kind != "notify_failed" || hang.n() != 3 || !strings.Contains(evs[0].Text, "deadline") {
		t.Fatalf("%+v after %d", evs[0], hang.n())
	}
}

// TestFailedCompletionRedelivers: if recording the outcome fails, the row is
// still in the outbox, and the next rescan delivers it again — at least once.
func TestFailedCompletionRedelivers(t *testing.T) {
	testutil.CheckGoroutines(t)
	clk := testutil.NewFakeClock(t0)
	ob := &memOutbox{failNextN: 1}
	ob.add(1, "n")
	n := &scripted{}
	d := &Dispatcher{Notifiers: map[string]Notifier{"n": n}, Store: ob, Clock: clk, Backoff: []time.Duration{}}
	stop := start(t, d)
	defer stop()
	testutil.Eventually(t, 5*time.Second, func() bool { return n.n() == 1 }, "first delivery")
	testutil.Eventually(t, 5*time.Second, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.pending) == 0
	}, "released")
	if _, evs := ob.snapshot(); len(evs) != 0 {
		t.Fatalf("outcome recorded despite failure: %v", evs)
	}
	clk.Advance(rescanInterval)
	evs := waitEvents(t, ob, 1)
	if n.n() != 2 || evs[0].Kind != "notify_ok" {
		t.Fatalf("redelivery: %d calls, %+v", n.n(), evs)
	}
}

func TestEnqueueDedupes(t *testing.T) {
	testutil.CheckGoroutines(t)
	ob := &memOutbox{}
	var calls atomic.Int32
	block := make(chan struct{})
	n := notifierFunc(func(ctx context.Context) error {
		calls.Add(1)
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	})
	d := &Dispatcher{Notifiers: map[string]Notifier{"n": n}, Store: ob, Backoff: []time.Duration{}}
	r := ob.add(1, "n")
	stop := start(t, d)
	defer stop()
	testutil.Eventually(t, 5*time.Second, func() bool { return calls.Load() == 1 }, "in flight")
	d.Enqueue(context.Background(), []meta.OutboxRow{r, r})
	d.rescan(context.Background())
	close(block)
	waitEvents(t, ob, 1)
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("an in-flight row was delivered %d times", calls.Load())
	}
}

type notifierFunc func(ctx context.Context) error

func (f notifierFunc) Notify(ctx context.Context, _ Notification) error { return f(ctx) }

// fakeSMTP accepts one session and records the DATA.
func fakeSMTP(t *testing.T, replies ...string) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 localhost ESMTP")
		var data, log strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			log.WriteString(cmd + "\n")
			if r := override(replies, cmd); r != "" {
				say(r)
				if strings.HasPrefix(r, "5") {
					got <- log.String()
					return
				}
				continue
			}
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-localhost")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH"):
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				got <- log.String() + "--\n" + data.String()
				return
			default:
				say("500 what")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestEmailSendsOverSMTP(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	e := &Email{Host: host, Port: p, User: "u", Password: "pw", From: "ozy@localhost", To: []string{"a@example.com", "b@example.com"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Notify(ctx, sample()); err != nil {
		t.Fatal(err)
	}
	session := <-got
	for _, want := range []string{"AUTH PLAIN", "MAIL FROM:<OZY@LOCALHOST>", "RCPT TO:<A@EXAMPLE.COM>", "RCPT TO:<B@EXAMPLE.COM>",
		"Subject: [ALERT] Job queue is backing up on queue:default"} {
		if !strings.Contains(session, want) {
			t.Errorf("session lacks %q:\n%s", want, session)
		}
	}
}

func TestEmailStalledServerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second) // never greets
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = (&Email{Host: "127.0.0.1", Port: p, From: "a@localhost", To: []string{"b@example.com"}}).Notify(ctx, sample())
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("stalled server: %v after %v", err, time.Since(start))
	}
}

// override finds a scripted reply ("RCPT=550 no such user") for cmd.
func override(replies []string, cmd string) string {
	for _, r := range replies {
		prefix, reply, _ := strings.Cut(r, "=")
		if strings.HasPrefix(cmd, prefix) {
			return reply
		}
	}
	return ""
}

func emailTo(addr string) *Email {
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return &Email{Host: host, Port: p, User: "u", Password: "pw", From: "ozy@localhost", To: []string{"a@example.com"}}
}

func TestEmailFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []struct {
		reply     string
		permanent bool
	}{
		{"AUTH=535 bad credentials", true},
		{"MAIL=550 sender refused", false},
		{"RCPT=550 no such user", false},
		{"DATA=554 no", false},
	} {
		addr, _ := fakeSMTP(t, c.reply)
		err := emailTo(addr).Notify(ctx, sample())
		if err == nil || IsPermanent(err) != c.permanent {
			t.Errorf("%s: %v (permanent %v)", c.reply, err, IsPermanent(err))
		}
	}
	// Nothing listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	if err := emailTo(addr).Notify(ctx, sample()); err == nil {
		t.Error("closed port")
	}
	// A bad From is permanent before anything is dialled.
	e := emailTo(addr)
	e.From = "nope"
	if err := e.Notify(ctx, sample()); !IsPermanent(err) {
		t.Errorf("bad from: %v", err)
	}
}

type failingOutbox struct{ memOutbox }

func (*failingOutbox) Outbox(context.Context) ([]meta.OutboxRow, error) {
	return nil, errors.New("db gone")
}

func TestRescanErrorIsSurvivable(t *testing.T) {
	testutil.CheckGoroutines(t)
	d := &Dispatcher{Notifiers: map[string]Notifier{}, Store: &failingOutbox{}}
	stop := start(t, d)
	stop()
}

// TestShutdownDuringBackoff: a cancelled dispatcher stops waiting between
// attempts instead of sleeping out the backoff.
func TestShutdownDuringBackoff(t *testing.T) {
	testutil.CheckGoroutines(t)
	ob := &memOutbox{}
	ob.add(1, "n")
	n := &scripted{errs: []error{errors.New("503"), errors.New("503")}}
	d := &Dispatcher{Notifiers: map[string]Notifier{"n": n}, Store: ob, Backoff: []time.Duration{time.Hour, time.Hour}}
	stop := start(t, d)
	testutil.Eventually(t, 5*time.Second, func() bool { return n.n() == 1 }, "first attempt")
	began := time.Now()
	stop()
	if time.Since(began) > 2*time.Second {
		t.Fatal("shutdown waited out the backoff")
	}
	if rows, evs := ob.snapshot(); len(rows) != 1 || len(evs) != 0 {
		t.Fatalf("rows %v events %v", rows, evs)
	}
}

func TestBuildMissingSecrets(t *testing.T) {
	none := func(string) string { return "" }
	for name, c := range map[string]config.Notifier{
		"url":     {Type: "webhook", URLEnv: "A"},
		"header":  {Type: "webhook", URL: "http://x", HeadersEnv: map[string]string{"Authorization": "B"}},
		"host":    {Type: "email", To: []string{"a@b.c"}, SMTP: config.SMTP{HostEnv: "C", Port: 25, From: "o@l"}},
		"user":    {Type: "email", To: []string{"a@b.c"}, SMTP: config.SMTP{Host: "h", Port: 25, From: "o@l", UserEnv: "D", PasswordEnv: "E"}},
		"unknown": {Type: "pager"},
	} {
		ns, infos := Build(map[string]config.Notifier{name: c}, none, nil)
		if _, ok := ns[name].(*Broken); !ok || infos[0].Ready || infos[0].Problem == "" {
			t.Errorf("%s: %T %+v", name, ns[name], infos)
		}
	}
	// With the password present but the user's variable set too, it builds.
	env := map[string]string{"D": "u", "E": "p"}
	ns, _ := Build(map[string]config.Notifier{"m": {Type: "email", To: []string{"a@b.c"},
		SMTP: config.SMTP{Host: "h", Port: 25, From: "o@l", UserEnv: "D", PasswordEnv: "E"}}}, func(k string) string { return env[k] }, nil)
	if e, ok := ns["m"].(*Email); !ok || e.User != "u" || e.Password != "p" {
		t.Errorf("email with auth: %+v", ns["m"])
	}
	nonePassword := map[string]string{"D": "u"}
	ns, _ = Build(map[string]config.Notifier{"m": {Type: "email", To: []string{"a@b.c"},
		SMTP: config.SMTP{Host: "h", Port: 25, From: "o@l", UserEnv: "D", PasswordEnv: "E"}}}, func(k string) string { return nonePassword[k] }, nil)
	if _, ok := ns["m"].(*Broken); !ok {
		t.Errorf("missing password built")
	}
}

func TestValuePtr(t *testing.T) {
	if ValuePtr(math.NaN()) != nil || ValuePtr(math.Inf(1)) != nil || *ValuePtr(2) != 2 {
		t.Fatal("ValuePtr")
	}
	if redact("x", "") != "x" || client(nil) != http.DefaultClient {
		t.Fatal("helpers")
	}
}
