// Command loadgen drives synthetic load at a running ozymandias, for the
// milestone acceptance numbers (docs/plan/testing.md L12).
//
//	loadgen -scenario statsd-flood -rate 50000 -duration 60s
//
// statsd-flood sends extended StatsD counters over UDP at a fixed message rate,
// batched into datagrams the way the SDKs batch them, spread over a number
// of distinct series. With -agent it reads the agent's /debug/vars before
// and after, and reports how many messages the agent parsed and how many it
// dropped — the only honest way to measure a UDP pipeline, since the sender
// never learns about loss.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	scenario := flag.String("scenario", "statsd-flood", "load scenario (statsd-flood)")
	addr := flag.String("addr", "127.0.0.1:8125", "agent statsd UDP address")
	agent := flag.String("agent", "http://127.0.0.1:8126", "agent HTTP base URL for /debug/vars (empty: don't measure)")
	rate := flag.Int("rate", 50000, "messages per second")
	duration := flag.Duration("duration", 60*time.Second, "how long to send")
	contexts := flag.Int("contexts", 100, "distinct series (route tag values)")
	batch := flag.Int("batch", 20, "messages per datagram")
	flag.Parse()
	if *scenario != "statsd-flood" {
		fmt.Fprintf(os.Stderr, "loadgen: unknown scenario %q\n", *scenario)
		os.Exit(2)
	}
	// Every one of these is a divisor or a loop bound below. A zero batch size
	// spins forever sending empty datagrams, and a zero context count divides
	// by zero — both are usage errors, so they exit 2 rather than panicking.
	for _, c := range []struct {
		name string
		v    int
	}{{"rate", *rate}, {"batch", *batch}, {"contexts", *contexts}} {
		if c.v < 1 {
			fmt.Fprintf(os.Stderr, "loadgen: -%s must be at least 1, got %d\n", c.name, c.v)
			os.Exit(2)
		}
	}
	if *duration <= 0 {
		fmt.Fprintf(os.Stderr, "loadgen: -duration must be positive, got %s\n", *duration)
		os.Exit(2)
	}
	if err := flood(*addr, *agent, *rate, *duration, *contexts, *batch); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func flood(addr, agent string, rate int, duration time.Duration, contexts, batch int) error {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	var before map[string]float64
	if agent != "" {
		if before, err = counters(agent); err != nil {
			return fmt.Errorf("reading agent counters: %w", err)
		}
	}

	// Pace in 10ms slices: rate/100 messages per slice, sent as datagrams of
	// `batch` lines.
	const slice = 10 * time.Millisecond
	perSlice := max(rate/100, 1)
	var sent, sendErrs int
	var buf strings.Builder
	start := time.Now()
	next := start
	for time.Since(start) < duration {
		for i := 0; i < perSlice; {
			buf.Reset()
			for j := 0; j < batch && i < perSlice; j, i = j+1, i+1 {
				if j > 0 {
					buf.WriteByte('\n')
				}
				fmt.Fprintf(&buf, "loadgen.flood:1|c|#route:/r%d", (sent+i)%contexts)
			}
			if _, err := conn.Write([]byte(buf.String())); err != nil {
				sendErrs++
			}
		}
		sent += perSlice
		next = next.Add(slice)
		time.Sleep(time.Until(next))
	}
	elapsed := time.Since(start)
	fmt.Printf("sent       %d messages in %s (%.0f msgs/s), %d datagrams failed to send\n",
		sent, elapsed.Round(time.Millisecond), float64(sent)/elapsed.Seconds(), sendErrs)

	if agent == "" {
		return nil
	}
	time.Sleep(2 * time.Second) // let the workers drain the queue
	after, err := counters(agent)
	if err != nil {
		return fmt.Errorf("reading agent counters: %w", err)
	}
	received := after["ozy.agent.statsd.messages_received"] - before["ozy.agent.statsd.messages_received"]
	dropped := after["ozy.agent.statsd.packets_dropped"] - before["ozy.agent.statsd.packets_dropped"]
	lost := float64(sent) - received
	fmt.Printf("received   %.0f messages (agent parsed)\n", received)
	fmt.Printf("dropped    %.0f datagrams in the agent's queue\n", dropped)
	fmt.Printf("lost       %.0f messages = %.4f%% (queue drops + kernel drops)\n", lost, 100*lost/float64(sent))
	return nil
}

// counters reads the agent's untagged counters from /debug/vars.
func counters(base string) (map[string]float64, error) {
	resp, err := http.Get(base + "/debug/vars") //nolint:gosec,noctx // operator-supplied URL; a CLI run
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Metrics []struct {
			Name  string   `json:"name"`
			Tags  []string `json:"tags"`
			Value *float64 `json:"value"`
		} `json:"metrics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, m := range body.Metrics {
		if m.Value != nil && len(m.Tags) == 0 {
			out[m.Name] = *m.Value
		}
	}
	return out, nil
}
