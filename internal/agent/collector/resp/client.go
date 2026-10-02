package resp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures a connection.
type Options struct {
	// Username and Password authenticate with AUTH after connecting. An
	// empty Password sends no AUTH; a Username needs Redis 6+ ACLs.
	Username string
	Password string
	// DB is selected with SELECT after AUTH when it is not 0.
	DB int
	// Timeout bounds the dial and each command whose context has no earlier
	// deadline. Default 5s. A check runs on a schedule with a timeout of its
	// own; this is the backstop for a caller that forgot one.
	Timeout time.Duration
	// Limits bound every reply; zero fields take [DefaultLimits].
	Limits Limits
}

// ErrClosed is returned by Do on a connection that was closed, or that a
// previous I/O or protocol error left unusable.
var ErrClosed = errors.New("resp: connection closed")

// Conn is one connection to a server. It is safe for concurrent use, but
// commands are serialized: RESP has no request ids, so the only way to match
// a reply to its request is order.
type Conn struct {
	nc      net.Conn
	r       *Reader
	timeout time.Duration

	mu     sync.Mutex
	buf    []byte
	broken error
}

// Dial connects to addr — "host:port", or a path starting with "/" for a
// unix socket — then authenticates and selects the database as opts say.
// A failure at any step closes the connection, and an AUTH refusal comes
// back as a [*ServerError] (Code WRONGPASS, NOAUTH, …) so the check can tell
// a wrong password from an unreachable server.
func Dial(ctx context.Context, addr string, opts Options) (*Conn, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	network := "tcp"
	if strings.HasPrefix(addr, "/") {
		network = "unix"
	}
	dctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(dctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("redis: dial %s: %w", addr, err)
	}
	c := &Conn{nc: nc, r: NewReader(nc, opts.Limits), timeout: opts.Timeout}
	if opts.Password != "" {
		args := []string{"AUTH", opts.Password}
		if opts.Username != "" {
			args = []string{"AUTH", opts.Username, opts.Password}
		}
		if _, err := c.Do(ctx, args...); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("redis: auth: %w", err)
		}
	}
	if opts.DB != 0 {
		if _, err := c.Do(ctx, "SELECT", strconv.Itoa(opts.DB)); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("redis: select %d: %w", opts.DB, err)
		}
	}
	return c, nil
}

// Do sends one command and reads its reply. An error reply returns the value
// and a [*ServerError]; the connection stays usable. Any other error — I/O, a
// deadline, [ErrProtocol], [ErrTooLarge] — breaks the connection, because
// the reply stream is no longer in step with the requests, and every later
// Do returns [ErrClosed].
//
// The deadline is the context's or Timeout from now, whichever is earlier;
// cancelling ctx interrupts a blocked read or write at once.
func (c *Conn) Do(ctx context.Context, args ...string) (Value, error) {
	if len(args) == 0 {
		return Value{}, errors.New("resp: empty command")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return Value{}, c.broken
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.nc.SetDeadline(deadline); err != nil {
		return Value{}, c.breakWith(err)
	}
	// A deadline in the past wakes any blocked read or write, so cancelling
	// ctx does not have to wait for the deadline to pass.
	stop := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c.buf = AppendCommand(c.buf[:0], args...)
	if _, err := c.nc.Write(c.buf); err != nil {
		return Value{}, c.breakWith(c.ctxErr(ctx, err))
	}
	v, err := c.r.Read()
	if err != nil {
		return Value{}, c.breakWith(c.ctxErr(ctx, err))
	}
	return v, v.Err()
}

// ctxErr prefers the context's error when the context is why the I/O failed,
// so a caller sees context.Canceled or DeadlineExceeded rather than an i/o
// timeout it never set. The socket deadline is the context's own deadline,
// and the socket can notice it before the context's timer has fired, so a
// timeout at or after that deadline counts too.
func (c *Conn) ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w (%w)", cerr, err)
	}
	var ne net.Error
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) && errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w (%w)", context.DeadlineExceeded, err)
	}
	return err
}

func (c *Conn) breakWith(err error) error {
	c.broken = fmt.Errorf("%w: after %w", ErrClosed, err)
	_ = c.nc.Close()
	return err
}

// Info runs INFO for the given sections (none means the server's default
// set) and parses the reply. A server answering with anything other than a
// string is a protocol error.
func (c *Conn) Info(ctx context.Context, sections ...string) (Info, error) {
	v, err := c.Do(ctx, append([]string{"INFO"}, sections...)...)
	if err != nil {
		return nil, err
	}
	s, ok := v.Text()
	if !ok {
		return nil, fmt.Errorf("%w: INFO answered with a %s", ErrProtocol, v.Kind)
	}
	return ParseInfo(s), nil
}

// Close closes the connection. It is safe to call more than once.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken == nil {
		c.broken = ErrClosed
		return c.nc.Close()
	}
	return nil
}
