package resp

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// step is one exchange the fake server expects: a command, then a raw reply.
// hang makes it read the command and never answer; drop makes it write reply
// and then close the connection.
type step struct {
	want  []string
	reply string
	hang  bool
	drop  bool
}

// fakeServer serves one connection from script, then closes. It reports a
// command that does not match the script as a test error: the point of a
// scripted server is that the client said exactly this.
func fakeServer(t *testing.T, network string, script ...step) string {
	t.Helper()
	addr := "127.0.0.1:0"
	if network == "unix" {
		// macOS limits a socket path to 104 bytes; t.TempDir is often longer.
		dir, err := os.MkdirTemp("", "resp")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		addr = filepath.Join(dir, "s")
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = ln.Close(); <-done })
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		r := NewReader(c, Limits{})
		for _, s := range script {
			cmd, err := r.Read()
			if err != nil {
				t.Errorf("fake server: reading a command: %v", err)
				return
			}
			var args []string
			for _, e := range cmd.Elems {
				args = append(args, e.Str)
			}
			if !reflect.DeepEqual(args, s.want) {
				t.Errorf("fake server: got %q, want %q", args, s.want)
				return
			}
			if s.hang {
				// Wait for the client to give up and close.
				_, _ = r.Read()
				return
			}
			if _, err := c.Write([]byte(s.reply)); err != nil {
				return
			}
			if s.drop {
				return
			}
		}
	}()
	return ln.Addr().String()
}

var ctx = context.Background()

func TestDial_AuthSelectAndInfo(t *testing.T) {
	info := "# Memory\r\nused_memory:100\r\n"
	addr := fakeServer(t, "tcp",
		step{want: []string{"AUTH", "ozy", "s3cret"}, reply: "+OK\r\n"},
		step{want: []string{"SELECT", "2"}, reply: "+OK\r\n"},
		step{want: []string{"INFO", "memory"}, reply: "$" + strconv.Itoa(len(info)) + "\r\n" + info + "\r\n"},
	)
	c, err := Dial(ctx, addr, Options{Username: "ozy", Password: "s3cret", DB: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	got, err := c.Info(ctx, "memory")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := got.Number("used_memory"); !ok || n != 100 {
		t.Fatalf("used_memory = %v, %v", n, ok)
	}
}

func TestDial_PasswordOnlyAuthAndNoSelectForDB0(t *testing.T) {
	addr := fakeServer(t, "tcp",
		step{want: []string{"AUTH", "pw"}, reply: "+OK\r\n"},
		step{want: []string{"PING"}, reply: "+PONG\r\n"},
	)
	c, err := Dial(ctx, addr, Options{Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if v, err := c.Do(ctx, "PING"); err != nil || v.Str != "PONG" {
		t.Fatalf("PING: %+v, %v", v, err)
	}
}

func TestDial_AuthRefused(t *testing.T) {
	for _, reply := range []string{
		"-WRONGPASS invalid username-password pair or user is disabled.\r\n",
		"-ERR AUTH <password> called without any password configured for the default user.\r\n",
	} {
		addr := fakeServer(t, "tcp", step{want: []string{"AUTH", "nope"}, reply: reply})
		_, err := Dial(ctx, addr, Options{Password: "nope"})
		var se *ServerError
		if !errors.As(err, &se) || !strings.HasPrefix(reply[1:], se.Code()) {
			t.Errorf("%q: err = %v, want the server's error", reply, err)
		}
	}
}

// Without AUTH, a protected server answers the first command with NOAUTH;
// that is a server error on a usable connection, and the check can report
// "needs a password" rather than "unreachable".
func TestDo_NoAuth(t *testing.T) {
	addr := fakeServer(t, "tcp",
		step{want: []string{"INFO"}, reply: "-NOAUTH Authentication required.\r\n"},
		step{want: []string{"PING"}, reply: "+PONG\r\n"},
	)
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.Info(ctx)
	var se *ServerError
	if !errors.As(err, &se) || se.Code() != "NOAUTH" {
		t.Fatalf("err = %v, want NOAUTH", err)
	}
	if _, err := c.Do(ctx, "PING"); err != nil {
		t.Fatalf("a server error must not break the connection: %v", err)
	}
}

func TestDial_SelectRefused(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"SELECT", "99"}, reply: "-ERR DB index is out of range\r\n"})
	if _, err := Dial(ctx, addr, Options{DB: 99}); err == nil || !strings.Contains(err.Error(), "select 99") {
		t.Fatalf("err = %v", err)
	}
}

func TestDial_Unreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if _, err := Dial(ctx, addr, Options{Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("err = %v", err)
	}
}

func TestDial_UnixSocket(t *testing.T) {
	addr := fakeServer(t, "unix", step{want: []string{"PING"}, reply: "+PONG\r\n"})
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Do(ctx, "PING"); err != nil {
		t.Fatal(err)
	}
}

// A slow server costs the deadline, not forever — and the connection is
// then unusable, because a late reply would be read as the answer to the
// next command.
func TestDo_DeadlineThenBroken(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"INFO"}, hang: true})
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	dctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Do(dctx, "INFO")
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to say the context's deadline passed", err)
	}
	if _, err := c.Do(ctx, "PING"); !errors.Is(err, ErrClosed) {
		t.Fatalf("after a timeout: %v, want ErrClosed", err)
	}
}

func TestDo_OptionsTimeoutIsTheBackstop(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"INFO"}, hang: true})
	c, err := Dial(ctx, addr, Options{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	if _, err := c.Do(ctx, "INFO"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestDo_CancelInterruptsABlockedRead(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"INFO"}, hang: true})
	c, err := Dial(ctx, addr, Options{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = c.Do(cctx, "INFO")
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v, want context.Canceled promptly", err, time.Since(start))
	}
}

func TestDo_AlreadyCancelled(t *testing.T) {
	addr := fakeServer(t, "tcp")
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Do(cctx, "PING"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Do(ctx); err == nil {
		t.Fatal("an empty command must be refused")
	}
}

func TestDo_ConnectionDropsMidReply(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"INFO"}, reply: "$100\r\n# Server\r\nredis_ver", drop: true})
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Info(ctx); err == nil {
		t.Fatal("a reply cut off mid-string must be an error")
	}
	if _, err := c.Do(ctx, "PING"); !errors.Is(err, ErrClosed) {
		t.Fatalf("after a dropped reply: %v, want ErrClosed", err)
	}
}

func TestDo_ProtocolErrorBreaks(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"PING"}, reply: "HTTP/1.1 400 Bad Request\r\n"})
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Do(ctx, "PING"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol (this is not a Redis server)", err)
	}
	if _, err := c.Do(ctx, "PING"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestDo_ReplyOverLimits(t *testing.T) {
	addr := fakeServer(t, "tcp", step{want: []string{"INFO"}, reply: "$99999999\r\n"})
	c, err := Dial(ctx, addr, Options{Limits: Limits{MaxBulk: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Info(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestInfo_WrongReplyKind(t *testing.T) {
	addr := fakeServer(t, "tcp",
		step{want: []string{"INFO"}, reply: ":1\r\n"},
		step{want: []string{"INFO"}, reply: "=16\r\ntxt:# X\r\nkey:1\r\n\r\n"},
	)
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Info(ctx); !errors.Is(err, ErrProtocol) {
		t.Fatalf("integer reply: %v, want ErrProtocol", err)
	}
	// A RESP3 server answers INFO with a verbatim string; it parses the same.
	got, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := got.Get("key"); v != "1" {
		t.Fatalf("verbatim INFO: %v", got)
	}
}

func TestClose_Twice(t *testing.T) {
	addr := fakeServer(t, "tcp")
	c, err := Dial(ctx, addr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Do(ctx, "PING"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Do after Close: %v", err)
	}
}
