package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults for [Options].
const (
	DefaultSocket = "/var/run/docker.sock"
	// DefaultTimeout bounds one non-streaming request. Stats are one-shot
	// and answer at once; the headroom is for a daemon under load.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxBodyBytes bounds a non-streaming response. A container
	// list of a few hundred containers is well under 1 MiB; this is a guard
	// against a broken daemon, not a limit anyone should meet.
	DefaultMaxBodyBytes = 8 << 20
	// MaxEventBytes bounds one line of the event stream. An event is a few
	// hundred bytes plus its container's labels.
	MaxEventBytes = 1 << 20
)

// ErrNotFound is what [Client.Stats] and [Client.Inspect] return, wrapped,
// when the daemon no longer knows the container. The normal case is a
// container that exited between the list and the stats call: the collector
// skips it, and treats every other error as a failure.
var ErrNotFound = errors.New("dockerapi: no such container")

// errEmptyBody is a 200 whose body is empty; [Client.Stats] treats it as
// a container that stopped mid-call.
var errEmptyBody = errors.New("empty response body")

// ErrStreamClosed is what [Client.Events] returns when the daemon ended the
// event stream (usually because it restarted). The caller reconnects.
var ErrStreamClosed = errors.New("dockerapi: event stream closed by the daemon")

// APIError is a non-2xx answer from the daemon. Docker puts a human-readable
// reason in {"message": "..."}; it is kept as sent.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dockerapi: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// Is makes errors.Is(err, ErrNotFound) true for a 404, so callers need not
// know status codes.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// Options configures a [Client]. Zero values get the defaults above.
type Options struct {
	// Socket is the daemon's unix socket.
	Socket string
	// Timeout bounds each non-streaming request. The event stream has no
	// timeout of its own (it is meant to stay open); its context bounds it.
	Timeout time.Duration
	// MaxBodyBytes bounds each non-streaming response body.
	MaxBodyBytes int64
}

// Client talks to one Docker daemon. It is safe for concurrent use; the
// underlying transport keeps a small pool of socket connections.
type Client struct {
	http    *http.Client
	timeout time.Duration
	maxBody int64
}

// New returns a Client for opts.Socket. Nothing is dialled until the first
// request, so a daemon that is down is an error from that request, not from
// New: the agent must start, and keep running, without Docker.
func New(opts Options) *Client {
	if opts.Socket == "" {
		opts.Socket = DefaultSocket
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	socket := opts.Socket
	tr := &http.Transport{
		// The URL's host is ignored: every connection goes to the socket.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		// Both at least the collector's stats concurrency, so a run's
		// connections are kept for the next rather than re-dialled.
		// MaxIdleConns caps the whole transport, so it must not be lower.
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	// No http.Client.Timeout: it would also cut the event stream. Each
	// non-streaming call gets a context deadline instead.
	return &Client{http: &http.Client{Transport: tr}, timeout: opts.Timeout, maxBody: opts.MaxBodyBytes}
}

// Close releases idle connections to the socket.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// ListContainers returns the running containers (GET /containers/json).
// Stopped ones are left out on purpose: the collector measures what is
// running and learns about exits from [Client.Events].
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var out []Container
	if err := c.getJSON(ctx, "/containers/json", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stats returns one snapshot of a container's resource usage
// (GET /containers/{id}/stats?stream=false&one-shot=true: one sample, so
// the call answers at once; see the package doc), and [Stats.Sampled] for
// the answer a container that stopped mid-call gets.
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var s Stats
	if err := checkID(id); err != nil {
		return s, err
	}
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/stats", q, &s)
	if errors.Is(err, errEmptyBody) {
		// A container that stops while the daemon takes its second CPU
		// sample can get a 200 with no body at all rather than zeros. Same
		// race, same answer: an unsampled Stats, not an error.
		return Stats{}, nil
	}
	return s, err
}

// Inspect returns the container's full description (GET
// /containers/{id}/json), of which only the state is decoded: the exit code
// and OOM flag behind a die event, and the start time behind uptime.
func (c *Client) Inspect(ctx context.Context, id string) (ContainerJSON, error) {
	var out ContainerJSON
	if err := checkID(id); err != nil {
		return out, err
	}
	err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &out)
	return out, err
}

// eventFilters asks the daemon for container start, oom and die events only,
// so the stream carries nothing the collector would discard. oom arrives
// just before the die of a container the kernel killed for memory, and is
// the only record of it once a --rm container is gone.
const eventFilters = `{"type":["container"],"event":["start","oom","die"]}`

// Events streams container start, oom and die events to fn until ctx is
// cancelled, the daemon closes the stream, or fn returns an error, and
// returns why it stopped: ctx's error, [ErrStreamClosed], fn's error, or a
// connection or decoding error. It always returns an error; there is no
// "done".
//
// since, when not zero, replays events from that instant on, inclusive:
// pass the last delivered event's [Event.At] to resume after a disconnect,
// and expect that event once more. Zero means "from now".
//
// fn runs on the calling goroutine, one event at a time. A slow fn holds
// the stream; the daemon buffers, and past its buffer, drops the client.
//
// A line that cannot be decoded, or is longer than MaxEventBytes, is
// skipped and reported to skipped (if not nil), not treated as the end of
// the stream: ending it would reconnect from the last good event, and the
// daemon would replay the same bad line, forever.
func (c *Client) Events(ctx context.Context, since time.Time, fn func(Event) error, skipped func(error)) error {
	q := url.Values{"filters": {eventFilters}}
	if !since.IsZero() {
		q.Set("since", formatSince(since))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("dockerapi: events: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dockerapi: GET /events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c.apiError(resp, http.MethodGet, "/events")
	}
	skip := func(err error) {
		if skipped != nil {
			skipped(err)
		}
	}
	br := bufio.NewReaderSize(resp.Body, 4096)
	for {
		line, tooLong, err := readLine(br, MaxEventBytes)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if errors.Is(err, io.EOF) {
				return ErrStreamClosed
			}
			return fmt.Errorf("dockerapi: reading /events: %w", err)
		}
		if tooLong {
			skip(fmt.Errorf("dockerapi: an event longer than %d bytes", MaxEventBytes))
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev, err := DecodeEvent(line)
		if err != nil {
			skip(err)
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// readLine reads one newline-terminated line, keeping at most limit bytes:
// a longer line is read to its end and discarded (tooLong), so one huge
// event costs no more memory than the limit. A final line without a
// newline is returned with io.EOF only if it is empty.
func readLine(br *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > limit {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			return line, tooLong, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && (len(line) > 0 || tooLong):
			return line, tooLong, nil
		default:
			return nil, false, err
		}
	}
}

// formatSince writes t as the daemon's since parameter: unix seconds with
// a nanosecond fraction, so resuming does not skip events in the same
// second as the last one seen.
func formatSince(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}

func checkID(id string) error {
	if id == "" {
		return errors.New("dockerapi: empty container id")
	}
	return nil
}

// getJSON GETs path with a per-request deadline and a bounded body, and
// decodes it into v.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("dockerapi: GET %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dockerapi: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c.apiError(resp, http.MethodGet, path)
	}
	body, err := c.readBody(resp.Body)
	if err != nil {
		return fmt.Errorf("dockerapi: GET %s: %w", path, err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("dockerapi: GET %s: %w", path, errEmptyBody)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("dockerapi: GET %s: decoding: %w", path, err)
	}
	return nil
}

func (c *Client) readBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, c.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("response larger than %d bytes", c.maxBody)
	}
	return body, nil
}

// apiError builds an APIError from a non-2xx response, reading at most a
// small prefix of the body for the daemon's message.
func (c *Client) apiError(resp *http.Response, method, path string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var body struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &body) == nil && body.Message != "" {
		msg = body.Message
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: msg}
}

// DecodeEvent parses one line of the event stream. Exported for the fuzz
// target and for callers replaying captured streams.
//
// Older daemons put the action and id at the top level ("status", "id");
// newer ones in "Action" and "Actor.ID", and send both. Either is accepted,
// and the result always has Action and Actor.ID filled when the input had
// either form.
func DecodeEvent(line []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, fmt.Errorf("dockerapi: decoding event: %w", err)
	}
	if ev.Action == "" {
		ev.Action = ev.Status
	}
	if ev.Actor.ID == "" {
		ev.Actor.ID = ev.ID
	}
	return ev, nil
}

// ExitCode returns a die event's exit code, from the exitCode attribute the
// daemon attaches. ok is false for other events and for a missing or
// unparsable attribute; a missing code is not exit code 0.
func (e Event) ExitCode() (code int, ok bool) {
	s, found := e.Actor.Attributes["exitCode"]
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
