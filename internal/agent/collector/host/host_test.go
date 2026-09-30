package host

import (
	"context"
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// canned is a Source that returns whatever the test last set.
type canned struct {
	cpu      cpu.TimesStat
	load     load.AvgStat
	mem      mem.VirtualMemoryStat
	swap     mem.SwapMemoryStat
	parts    []disk.PartitionStat
	usage    map[string]*disk.UsageStat
	io       map[string]disk.IOCountersStat
	net      []net.IOCountersStat
	uptime   uint64
	failWith map[string]error // group name → error
}

func (c *canned) err(group string) error { return c.failWith[group] }

func (c *canned) CPUTimes(context.Context) (cpu.TimesStat, error) { return c.cpu, c.err("cpu") }
func (c *canned) Load(context.Context) (*load.AvgStat, error)     { return &c.load, c.err("load") }
func (c *canned) VirtualMemory(context.Context) (*mem.VirtualMemoryStat, error) {
	return &c.mem, c.err("mem")
}
func (c *canned) SwapMemory(context.Context) (*mem.SwapMemoryStat, error) {
	return &c.swap, c.err("swap")
}
func (c *canned) Partitions(context.Context) ([]disk.PartitionStat, error) {
	return c.parts, c.err("parts")
}
func (c *canned) DiskUsage(_ context.Context, path string) (*disk.UsageStat, error) {
	if u, ok := c.usage[path]; ok {
		return u, nil
	}
	return nil, errors.New("permission denied")
}
func (c *canned) DiskIO(context.Context) (map[string]disk.IOCountersStat, error) {
	return c.io, c.err("io")
}
func (c *canned) NetIO(context.Context) ([]net.IOCountersStat, error) { return c.net, c.err("net") }
func (c *canned) Uptime(context.Context) (uint64, error)              { return c.uptime, c.err("uptime") }

var t0 = time.Unix(1_790_000_000, 0)

func machine() *canned {
	return &canned{
		cpu:  cpu.TimesStat{User: 100, Nice: 10, System: 50, Idle: 800, Iowait: 20, Irq: 5, Softirq: 5, Steal: 10, Guest: 40, GuestNice: 5},
		load: load.AvgStat{Load1: 1.5, Load5: 1, Load15: 0.5},
		mem:  mem.VirtualMemoryStat{Total: 1000, Used: 600, Free: 100, Available: 300},
		swap: mem.SwapMemoryStat{Total: 200, Used: 50, Free: 150},
		parts: []disk.PartitionStat{
			{Device: "/dev/sda1", Mountpoint: "/", Fstype: "ext4"},
			{Device: "/dev/sda1", Mountpoint: "/var/lib/docker", Fstype: "ext4"}, // bind mount: counted once
			{Device: "devfs", Mountpoint: "/dev", Fstype: "devfs"},
			{Device: "/dev/sdb1", Mountpoint: "/secret", Fstype: "ext4"}, // unreadable
		},
		usage: map[string]*disk.UsageStat{
			"/":               {Total: 1000, Used: 250, Free: 750, UsedPercent: 25},
			"/var/lib/docker": {Total: 1000, Used: 250, Free: 750, UsedPercent: 25},
			"/dev":            {Total: 10, Used: 10, UsedPercent: 100},
		},
		io: map[string]disk.IOCountersStat{"sda": {ReadCount: 100, WriteCount: 200, ReadBytes: 1024 * 100, WriteBytes: 1024 * 300}},
		net: []net.IOCountersStat{
			{Name: "eth0", BytesRecv: 1000, BytesSent: 2000, PacketsRecv: 10, PacketsSent: 20},
			{Name: "utun3", BytesRecv: 5, PacketsRecv: 1},
			{Name: "idle0"}, // never moved a packet
		},
		uptime: 3600,
	}
}

type got map[string][]collector.Metric

func (g got) one(t *testing.T, name string) collector.Metric {
	t.Helper()
	if len(g[name]) != 1 {
		t.Fatalf("%s: %d metrics, want 1: %+v", name, len(g[name]), g[name])
	}
	return g[name][0]
}

func run(t *testing.T, c *Collector) (got, error) {
	t.Helper()
	out := got{}
	err := c.Collect(context.Background(), func(m collector.Metric) { out[m.Name] = append(out[m.Name], m) })
	return out, err
}

func newCollector(src Source, fc *testutil.FakeClock) *Collector {
	return New(Options{Source: src, Clock: fc, ExcludeInterfaces: []*regexp.Regexp{regexp.MustCompile(`^(utun|awdl|llw|anpi|gif|stf)[0-9]+$`)}})
}

func TestHost_FirstRunHasGaugesButNoRatesOrCPU(t *testing.T) {
	c := newCollector(machine(), testutil.NewFakeClock(t0))
	g, err := run(t, c)
	if err == nil || !strings.Contains(err.Error(), "/secret") {
		t.Fatalf("err = %v, want the unreadable partition named", err)
	}
	for name := range g {
		if strings.HasPrefix(name, "system.cpu.") || strings.HasPrefix(name, "system.io.") || strings.HasPrefix(name, "system.net.") {
			t.Errorf("%s on the first run: a rate or percentage needs two readings", name)
		}
	}
	if m := g.one(t, "system.load.1"); m.Value != 1.5 || m.Kind != collector.Gauge {
		t.Errorf("load.1 = %+v", m)
	}
	if v := g.one(t, "system.mem.pct_usable").Value; v != 0.3 {
		t.Errorf("pct_usable = %v, want 0.3 (available/total, not free/total)", v)
	}
	if v := g.one(t, "system.swap.pct_free").Value; v != 0.75 {
		t.Errorf("swap pct_free = %v", v)
	}
	if v := g.one(t, "system.uptime").Value; v != 3600 {
		t.Errorf("uptime = %v", v)
	}
	// One device, once: not the bind mount again, not devfs, not the
	// unreadable one.
	if m := g.one(t, "system.disk.in_use"); m.Value != 0.25 || !slices.Equal(m.Tags, []string{"device:/dev/sda1"}) {
		t.Errorf("disk.in_use = %+v", m)
	}
}

func TestHost_SecondRunHasCPUPercentagesAndRates(t *testing.T) {
	src := machine()
	fc := testutil.NewFakeClock(t0)
	c := newCollector(src, fc)
	_, _ = run(t, c)

	// 15s later. CPU: 100 more ticks across states; guest time grew too, but
	// it is already inside user and must not inflate the total.
	fc.Advance(15 * time.Second)
	src.cpu.User += 40
	src.cpu.Nice += 10
	src.cpu.System += 10
	src.cpu.Irq += 5
	src.cpu.Softirq += 5
	src.cpu.Idle += 20
	src.cpu.Iowait += 5
	src.cpu.Steal += 5
	src.cpu.Guest += 30
	src.io["sda"] = disk.IOCountersStat{ReadCount: 250, WriteCount: 200, ReadBytes: 1024 * 400, WriteBytes: 1024 * 300}
	src.net[0].BytesRecv += 15000
	src.net[0].PacketsRecv += 15
	g, _ := run(t, c)

	want := map[string]float64{
		"system.cpu.user": 50, "system.cpu.system": 20, "system.cpu.idle": 20,
		"system.cpu.iowait": 5, "system.cpu.stolen": 5,
		"system.io.r_s": 10, "system.io.w_s": 0, "system.io.rkb_s": 20, "system.io.wkb_s": 0,
		"system.net.bytes_rcvd": 1000, "system.net.packets_in.count": 1, "system.net.bytes_sent": 0,
	}
	for name, w := range want {
		m := g.one(t, name)
		if math.Abs(m.Value-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, m.Value, w)
		}
	}
	var sum float64
	for _, n := range []string{"user", "system", "idle", "iowait", "stolen"} {
		sum += g.one(t, "system.cpu."+n).Value
	}
	if math.Abs(sum-100) > 1e-9 {
		t.Errorf("CPU states sum to %v, want 100", sum)
	}
	if m := g.one(t, "system.net.bytes_rcvd"); m.Kind != collector.Rate || !slices.Equal(m.Tags, []string{"interface:eth0"}) {
		t.Errorf("bytes_rcvd = %+v", m)
	}
	for _, m := range g["system.net.bytes_rcvd"] {
		if m.Tags[0] != "interface:eth0" {
			t.Errorf("an excluded or idle interface was reported: %v", m.Tags)
		}
	}
}

func TestHost_ACounterResetSkipsOneRateAndNoMore(t *testing.T) {
	src := machine()
	fc := testutil.NewFakeClock(t0)
	c := newCollector(src, fc)
	_, _ = run(t, c)
	fc.Advance(15 * time.Second)
	src.net[0].BytesRecv = 10 // the interface was reset
	g, _ := run(t, c)
	if len(g["system.net.bytes_rcvd"]) != 0 {
		t.Fatalf("a rate across a reset: %+v", g["system.net.bytes_rcvd"])
	}
	fc.Advance(15 * time.Second)
	src.net[0].BytesRecv = 160
	g, _ = run(t, c)
	if v := g.one(t, "system.net.bytes_rcvd").Value; v != 10 {
		t.Fatalf("after the reset: %v, want 10", v)
	}
}

func TestHost_CPUCountersGoingBackYieldNothing(t *testing.T) {
	src := machine()
	fc := testutil.NewFakeClock(t0)
	c := newCollector(src, fc)
	_, _ = run(t, c)
	src.cpu = cpu.TimesStat{User: 1, Idle: 1} // a restored VM
	g, _ := run(t, c)
	if len(g["system.cpu.user"]) != 0 {
		t.Fatalf("percentages across a counter reset: %+v", g["system.cpu.user"])
	}
}

func TestHost_AFailingGroupDoesNotHideTheOthers(t *testing.T) {
	src := machine()
	src.failWith = map[string]error{"mem": errors.New("boom"), "io": errors.ErrUnsupported}
	c := newCollector(src, testutil.NewFakeClock(t0))
	g, err := run(t, c)
	if err == nil || !strings.Contains(err.Error(), "memory: boom") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "io: ") {
		t.Errorf("an unsupported group was reported as a failure: %v", err)
	}
	if len(g["system.load.1"]) != 1 || len(g["system.uptime"]) != 1 {
		t.Errorf("groups after the failing one were skipped: %v", g)
	}
}

func TestHost_EveryGroupErrorIsReported(t *testing.T) {
	for _, group := range []string{"cpu", "load", "mem", "swap", "parts", "io", "net", "uptime"} {
		src := machine()
		src.failWith = map[string]error{group: errors.New("x")}
		if _, err := run(t, newCollector(src, testutil.NewFakeClock(t0))); err == nil {
			t.Errorf("%s: failure not reported", group)
		}
	}
}

func TestHost_StopsWhenCancelled(t *testing.T) {
	c := newCollector(machine(), testutil.NewFakeClock(t0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	err := c.Collect(ctx, func(collector.Metric) { n++ })
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("err = %v, emitted %d", err, n)
	}
}

// A device or interface that disappears is forgotten, so the rate tracker
// holds what exists now rather than everything that ever did.
func TestHost_ForgetsWhatDisappears(t *testing.T) {
	src := machine()
	fc := testutil.NewFakeClock(t0)
	c := newCollector(src, fc)
	_, _ = run(t, c)
	before := c.rates.Len()
	src.net = src.net[:0]
	src.io = map[string]disk.IOCountersStat{}
	fc.Advance(forgetAfter + time.Second)
	_, _ = run(t, c)
	if c.rates.Len() != 0 || before == 0 {
		t.Fatalf("tracked %d keys before, %d after everything went away", before, c.rates.Len())
	}
}

func TestHost_NameAndInterval(t *testing.T) {
	c := New(Options{Interval: 30 * time.Second})
	if c.Name() != "host" || c.Interval() != 30*time.Second {
		t.Fatalf("%q %v", c.Name(), c.Interval())
	}
}

// The real machine, briefly: every group either works here or is
// unsupported, and nothing is emitted that the wire would refuse.
func TestHost_RealMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("reads the real machine")
	}
	c := New(Options{})
	for range 2 {
		err := c.Collect(context.Background(), func(m collector.Metric) {
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) || m.Value < 0 && m.Kind == collector.Rate {
				t.Errorf("%s = %v", m.Name, m.Value)
			}
		})
		// A partition this user may not read is an environment fact, not a
		// bug; anything else is.
		if err != nil && !strings.Contains(err.Error(), "disk:") {
			t.Errorf("collect: %v", err)
		}
	}
}
