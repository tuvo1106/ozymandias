package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Notification is one message about one group of one monitor: what the
// outbox stores, and what every notifier is handed. Rendered already — the
// template ran when the notification was owed, against the state it describes,
// so a delivery retried an hour later still says what was true then.
type Notification struct {
	MonitorID   int64             `json:"monitor_id"`
	MonitorUID  string            `json:"monitor_uid,omitempty"`
	MonitorName string            `json:"monitor_name"`
	Tags        []string          `json:"tags,omitempty"`
	GroupKey    string            `json:"group_key"`
	Group       map[string]string `json:"group"`
	Status      string            `json:"status"`
	PrevStatus  string            `json:"prev_status"`
	// Value is nil for a NO_DATA notification: there is no value, and 0 would
	// be a lie a reader could act on.
	Value     *float64  `json:"value"`
	Threshold float64   `json:"threshold"`
	Reason    string    `json:"reason"` // transition | renotify | test
	Message   string    `json:"message"`
	Link      string    `json:"link,omitempty"`
	Ts        time.Time `json:"ts"`
}

// ValuePtr returns v as a Value, nil for NaN.
func ValuePtr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// Title is the one-line summary every notifier leads with.
func (n Notification) Title() string {
	prefix := "[" + n.Status + "]"
	if n.Status == "OK" {
		prefix = "[Recovered]"
	}
	if n.Reason == "test" {
		prefix = "[Test]"
	}
	t := prefix + " " + n.MonitorName
	if n.GroupKey != "" && n.GroupKey != "*" {
		t += " on " + n.GroupKey
	}
	return t
}

// Notifier delivers a notification somewhere. Notify must honour ctx: the
// dispatcher bounds every attempt with it.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Permanent wraps an error that retrying cannot fix — a 400 from a webhook, a
// notifier whose secret is not configured — so the dispatcher stops at the
// first attempt instead of spending two more on it.
type Permanent struct{ Err error }

func (p *Permanent) Error() string { return p.Err.Error() }
func (p *Permanent) Unwrap() error { return p.Err }

// IsPermanent reports whether err is (or wraps) a [Permanent].
func IsPermanent(err error) bool {
	var p *Permanent
	return errors.As(err, &p)
}

// postJSON is the shared HTTP delivery: POST body, 2xx is success, 429 and 5xx
// are worth retrying, any other status is permanent.
//
// The response body is read (at most 1 KiB) only to put it in the error: a
// Discord 400 says which field it disliked, which is the whole diagnosis.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return &Permanent{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return &Permanent{fmt.Errorf("building request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ozymandias-monitor")
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		req.Header.Set(k, headers[k])
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL may carry a token (a Discord webhook's does); url.Error
		// prints it. Report the failure without it.
		return errors.New(redact(err.Error(), url))
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return err
	}
	return &Permanent{err}
}

// redact removes a secret-bearing URL from an error message.
func redact(msg, url string) string {
	if url == "" {
		return msg
	}
	return strings.ReplaceAll(msg, url, "<redacted url>")
}

// Webhook POSTs the notification as JSON. The body is [Notification]'s JSON,
// documented in docs/operations.md#webhook-payload.
type Webhook struct {
	URL     string
	Headers map[string]string
	Client  *http.Client
}

// Notify implements Notifier.
func (w *Webhook) Notify(ctx context.Context, n Notification) error {
	return postJSON(ctx, client(w.Client), w.URL, w.Headers, n)
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return http.DefaultClient
}

// Discord posts an embed to a Discord webhook.
type Discord struct {
	URL    string
	Client *http.Client
}

// Discord's limits on an embed; past them the whole message is refused with a
// 400, so text is cut to fit rather than sent to fail.
const (
	discordTitleMax = 256
	discordDescMax  = 4096
)

// Embed colours by status.
var discordColors = map[string]int{"ALERT": 0xD32F2F, "WARN": 0xF9A825, "NO_DATA": 0x757575, "OK": 0x2E7D32}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	URL         string         `json:"url,omitempty"`
	Color       int            `json:"color"`
	Timestamp   string         `json:"timestamp"`
	Fields      []discordField `json:"fields"`
}

type discordBody struct {
	Username string         `json:"username"`
	Embeds   []discordEmbed `json:"embeds"`
	// AllowedMentions is empty: a message template is not a way to @everyone
	// a Discord server.
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

// DiscordBody builds the request body. Exported for the golden test.
func DiscordBody(n Notification) any {
	value := "no data"
	if n.Value != nil {
		value = fmt.Sprintf("%g", *n.Value)
	}
	e := discordEmbed{
		Title:       truncate(n.Title(), discordTitleMax),
		Description: truncate(n.Message, discordDescMax),
		URL:         n.Link,
		Color:       discordColors[n.Status],
		Timestamp:   n.Ts.UTC().Format(time.RFC3339),
		Fields: []discordField{
			{Name: "Status", Value: n.PrevStatus + " → " + n.Status, Inline: true},
			{Name: "Value", Value: value, Inline: true},
			{Name: "Threshold", Value: fmt.Sprintf("%g", n.Threshold), Inline: true},
		},
	}
	b := discordBody{Username: "ozymandias", Embeds: []discordEmbed{e}}
	b.AllowedMentions.Parse = []string{}
	return b
}

// Notify implements Notifier.
func (d *Discord) Notify(ctx context.Context, n Notification) error {
	return postJSON(ctx, client(d.Client), d.URL, nil, DiscordBody(n))
}

// truncate cuts s to at most max runes, marking the cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// Log writes the notification to ozyd's own log. It is the notifier that
// cannot fail, for development and for the scenario tests.
type Log struct{ Logger *slog.Logger }

// Notify implements Notifier.
func (l *Log) Notify(_ context.Context, n Notification) error {
	lg := l.Logger
	if lg == nil {
		lg = slog.Default()
	}
	lg.Info("monitor notification", "title", n.Title(), "monitor_id", n.MonitorID, "group", n.GroupKey,
		"status", n.Status, "prev_status", n.PrevStatus, "reason", n.Reason, "message", n.Message)
	return nil
}

// Broken is a notifier that could not be built — its secret's environment
// variable is unset, say. It is kept rather than dropped so that a message
// mentioning it records a notify_failed event with the reason, where a dropped
// one would be a notification that silently went nowhere.
type Broken struct{ Reason error }

// Notify implements Notifier.
func (b *Broken) Notify(context.Context, Notification) error { return &Permanent{b.Reason} }
