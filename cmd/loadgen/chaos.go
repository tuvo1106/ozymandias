package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The chaos scenarios give the drills in scripts/chaos an oracle. A drill kills
// or starves ozyd while ack-ingest sends; verify then reads the stores back and
// compares them with the acks file, the only record of what ozyd promised.
//
// What an acknowledgement means: a request answered 202 is a promise (it is in the
// queue, or stored). Anything else, a refusal or a connection that died, is
// "unknown": the request may or may not have landed, and the sender, which is
// deliberately not retrying, cannot tell. So the contract verify checks is
//
//	every acked log is stored exactly once      (no loss, no duplicate)
//	every stored log was acked or unknown       (nothing invented)
//	acked series points <= stored <= acked + unknown
//
// Logs carry a sequence number, so their duplicates are visible one by one. Series
// points of one lane share a series, so only the bound can be checked.

const (
	chaosService = "chaos"
	chaosMetric  = "chaos.points"
	chaosLanes   = 64
)

// ackLine is one line of the acks file: "<kind> <seq> <outcome>", kind s (series)
// or l (log), outcome a (acked) or u (unknown).
type ackLine struct {
	kind    byte
	seq     int
	acked   bool
	tsFirst int64
}

func ackIngest(ozyd, acksPath string, rate int, duration time.Duration) error {
	f, err := os.OpenFile(acksPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f) // flushed after every sequence number below

	// Continue the sequence of an earlier run into the same file: a drill runs this
	// several times (once per kill) and every seq must stay unique.
	seq, err := nextSeq(acksPath)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	// Seeded from the acks file, like seq: a second run into the same file must not send a lane a
	// timestamp at or before one an earlier run already had acknowledged, which the store would drop
	// as out of order and verify would then report as lost.
	lastTS := make([]int64, chaosLanes)
	prior, err := readAcks(acksPath)
	if err != nil {
		return err
	}
	for _, a := range prior {
		if a.kind == 's' {
			lastTS[a.seq%chaosLanes] = max(lastTS[a.seq%chaosLanes], a.tsFirst)
		}
	}
	tick := time.NewTicker(time.Second / time.Duration(rate))
	defer tick.Stop()
	deadline := time.Now().Add(duration)
	var acked, unknown int
	for time.Now().Before(deadline) {
		<-tick.C
		lane := seq % chaosLanes
		ts := max(time.Now().Unix(), lastTS[lane]+1)
		lastTS[lane] = ts
		series := fmt.Sprintf(`{"series":[{"metric":%q,"type":"gauge","interval":0,"tags":["lane:%d"],"points":[[%d,1]]}]}`, chaosMetric, lane, ts)
		logs := fmt.Sprintf(`{"logs":[{"ts":%d,"message":"chaos %d","status":"info","service":%q}]}`, time.Now().UnixMilli(), seq, chaosService)
		for _, r := range []struct {
			kind byte
			path string
			body string
		}{{'s', "/v1/series", series}, {'l', "/v1/logs", logs}} {
			ok := post(client, ozyd+r.path, r.body)
			outcome := byte('u')
			if ok {
				outcome = 'a'
				acked++
			} else {
				unknown++
			}
			fmt.Fprintf(w, "%c %d %c %d\n", r.kind, seq, outcome, ts)
		}
		// Flushed per sequence number: this process is not the one being killed, but a
		// drill may stop it, and an ack that is not in the file does not exist.
		if err := w.Flush(); err != nil {
			return err
		}
		seq++
	}
	fmt.Printf("sent %d acked, %d unknown (next seq %d)\n", acked, unknown, seq)
	return nil
}

// post reports whether ozyd acknowledged body: a 202 that accepted something and rejected nothing.
func post(c *http.Client, url, body string) bool { return postWhy(c, url, body) == nil }

// postWhy is post with the reason it was not an acknowledgement: a transport error, a status
// other than 202, or a 202 that rejected items, each with what the server said.
//
// A 202 whose body says something was rejected is not a promise about that something: the
// first version of the chaos drills sent a type ozyd refuses, got 202s and "verified" nothing
// was lost because it believed 2428 acks that were refusals.
func postWhy(c *http.Client, url, body string) error {
	resp, err := c.Post(url, "application/json", strings.NewReader(body)) //nolint:gosec,noctx // operator-supplied URL; a CLI run
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s: %.200s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var reply struct{ Accepted, Rejected int }
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("202 with an unreadable body: %.200s", raw)
	}
	if reply.Rejected != 0 || reply.Accepted == 0 {
		return fmt.Errorf("202 with accepted=%d rejected=%d: %.200s", reply.Accepted, reply.Rejected, raw)
	}
	return nil
}

func readAcks(path string) ([]ackLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []ackLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) != 4 {
			return nil, fmt.Errorf("acks file: malformed line %q", sc.Text())
		}
		seq, err1 := strconv.Atoi(p[1])
		ts, err2 := strconv.ParseInt(p[3], 10, 64)
		if err := errors.Join(err1, err2); err != nil || len(p[0]) != 1 || len(p[2]) != 1 {
			return nil, fmt.Errorf("acks file: malformed line %q", sc.Text())
		}
		out = append(out, ackLine{kind: p[0][0], seq: seq, acked: p[2][0] == 'a', tsFirst: ts})
	}
	return out, sc.Err()
}

func nextSeq(path string) (int, error) {
	acks, err := readAcks(path)
	if err != nil {
		return 0, err
	}
	next := 0
	for _, a := range acks {
		next = max(next, a.seq+1)
	}
	return next, nil
}

// verify reads the stores back and reports every way they disagree with the acks.
// It returns an error (exit 1) if any acked data is missing or any log is stored
// twice, so a drill can assert on its exit status.
func verify(ozyd, acksPath string, settle time.Duration) error {
	acks, err := readAcks(acksPath)
	if err != nil {
		return err
	}
	if len(acks) == 0 {
		return errors.New("the acks file is empty: nothing was sent, so nothing was checked")
	}
	var ackedLogs, unknownLogs = map[int]bool{}, map[int]bool{}
	var ackedSeries, unknownSeries int
	lo, hi := acks[0].tsFirst, acks[0].tsFirst
	for _, a := range acks {
		lo, hi = min(lo, a.tsFirst), max(hi, a.tsFirst)
		switch {
		case a.kind == 'l' && a.acked:
			ackedLogs[a.seq] = true
		case a.kind == 'l':
			unknownLogs[a.seq] = true
		case a.acked:
			ackedSeries++
		default:
			unknownSeries++
		}
	}

	// Queued mode applies after it acknowledges: wait for the stores to catch up
	// to the acked set (or give up after settle and report what is missing).
	deadline := time.Now().Add(settle)
	for {
		stored, dup, err := storedLogs(ozyd, (lo-120)*1000, (hi+120)*1000)
		if err != nil {
			return err
		}
		points, err := storedPoints(ozyd, lo-120, hi+120)
		if err != nil {
			return err
		}
		var missing, invented []int
		for s := range ackedLogs {
			if stored[s] == 0 {
				missing = append(missing, s)
			}
		}
		for s := range stored {
			if !ackedLogs[s] && !unknownLogs[s] {
				invented = append(invented, s)
			}
		}
		sort.Ints(missing)
		sort.Ints(invented)
		seriesLow := points < float64(ackedSeries)
		if (len(missing) == 0 && !seriesLow) || time.Now().After(deadline) {
			fmt.Printf("logs:   %d acked, %d unknown, %d stored; missing %d, stored twice %d, never sent %d\n",
				len(ackedLogs), len(unknownLogs), len(stored), len(missing), len(dup), len(invented))
			fmt.Printf("series: %d acked, %d unknown, %.0f stored\n", ackedSeries, unknownSeries, points)
			var problems []string
			if len(missing) > 0 {
				problems = append(problems, fmt.Sprintf("%d acknowledged logs are missing (first: %v)", len(missing), head(missing)))
			}
			if len(dup) > 0 {
				problems = append(problems, fmt.Sprintf("%d logs are stored more than once (first: %v)", len(dup), head(dup)))
			}
			if len(invented) > 0 {
				problems = append(problems, fmt.Sprintf("%d stored logs were never sent (first: %v)", len(invented), head(invented)))
			}
			if seriesLow {
				problems = append(problems, fmt.Sprintf("%d acknowledged points are missing", ackedSeries-int(points)))
			}
			if points > float64(ackedSeries+unknownSeries) {
				problems = append(problems, fmt.Sprintf("%.0f points stored but only %d were acked or unknown: duplicates", points, ackedSeries+unknownSeries))
			}
			if len(problems) > 0 {
				return errors.New(strings.Join(problems, "; "))
			}
			fmt.Println("ok: nothing acknowledged was lost, nothing was stored twice")
			return nil
		}
		time.Sleep(time.Second)
	}
}

func head(s []int) []int { return s[:min(len(s), 5)] }

// storedLogs pages through every chaos log in [from, to] (unix ms) and counts how
// many times each sequence number is stored. dup lists those stored more than once.
func storedLogs(ozyd string, from, to int64) (stored map[int]int, dup []int, err error) {
	stored = map[int]int{}
	cursor := ""
	pages := 0
	for {
		q := url.Values{"q": {"service:" + chaosService}, "from": {fmt.Sprint(from)}, "to": {fmt.Sprint(to)}, "limit": {"5000"}, "order": {"asc"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var res struct {
			Logs []struct {
				Message string `json:"message"`
			} `json:"logs"`
			Cursor string `json:"cursor"`
		}
		if err := getJSON(ozyd+"/api/v1/logs?"+q.Encode(), &res); err != nil {
			return nil, nil, err
		}
		for _, l := range res.Logs {
			if n, err := strconv.Atoi(strings.TrimPrefix(l.Message, "chaos ")); err == nil {
				stored[n]++
			}
		}
		// An empty page with a cursor is a search that ran out of scan budget, not the end: only
		// the absence of a cursor is. (Stopping on an empty page reported data that exists as missing.)
		if res.Cursor == "" {
			break
		}
		if pages++; pages > 100000 {
			return nil, nil, errors.New("log search never ended")
		}
		cursor = res.Cursor
	}
	for s, n := range stored {
		if n > 1 {
			dup = append(dup, s)
		}
	}
	sort.Ints(dup)
	return stored, dup, nil
}

// queryWindow is the most seconds one storedPoints query covers: one bucket each at interval 1,
// under the API's limit of 1500 buckets. A drill that acknowledges faster runs over more seconds
// (the first version of this oracle asked for the whole range at once and failed at 1541).
const queryWindow = 1000

// storedPoints is how many chaos points are stored: the sum of a gauge that is 1
// at every point, over every lane, for the seconds from..to inclusive. It asks in windows of at
// most queryWindow seconds that tile the range exactly (the API's from and to are both inclusive,
// so a window starts the second after the last one ended), and never in a window of one second,
// which the API refuses.
func storedPoints(ozyd string, from, to int64) (float64, error) {
	if to <= from {
		return 0, fmt.Errorf("storedPoints: %d..%d is not a range", from, to)
	}
	var sum float64
	for a := from; a <= to; {
		b := min(a+queryWindow-1, to)
		if to-b == 1 { // the next window would be one second wide
			b--
		}
		n, err := storedPointsIn(ozyd, a, b)
		if err != nil {
			return 0, err
		}
		sum += n
		a = b + 1
	}
	return sum, nil
}

func storedPointsIn(ozyd string, from, to int64) (float64, error) {
	q := url.Values{"metric": {chaosMetric}, "agg": {"sum"}, "from": {fmt.Sprint(from)}, "to": {fmt.Sprint(to)}, "interval": {"1"}}
	var res struct {
		Series []struct {
			Points [][]*float64 `json:"points"`
		} `json:"series"`
	}
	if err := getJSON(ozyd+"/api/v1/query?"+q.Encode(), &res); err != nil {
		return 0, err
	}
	var sum float64
	for _, s := range res.Series {
		for _, p := range s.Points {
			if len(p) == 2 && p[1] != nil {
				sum += *p[1]
			}
		}
	}
	return sum, nil
}

func getJSON(u string, into any) error {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(u) //nolint:gosec,noctx // operator-supplied URL; a CLI run
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d %s", u, resp.StatusCode, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, into)
}
