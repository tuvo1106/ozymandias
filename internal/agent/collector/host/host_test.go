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
		// What a container sees: an overlay root, pseudo filesystems, and
		// the real disk only through bind mounts.
		parts: []disk.PartitionStat{
			{Device: "overlay", Mountpoint: "/", Fstype: "overlay"},
			{Device: "proc", Mountpoint: "/proc", Fstype: "proc"},
			{Device: "devfs", Mountpoint: "/dev", Fstype: "devfs"},
			{Device: "/dev/sda1", Mountpoint: "/etc/hosts", Fstype: "ext4", Opts: []string{"rw", "bind"}},
			{Device: "/dev/sda1", Mountpoint: "/etc/resolv.conf", Fstype: "ext4", Opts: []string{"rw", "bind"}}, // counted once
			{Device: "nas:/export", Mountpoint: "/mnt/nas", Fstype: "nfs"},                                      // a network mount: never statfs'd
			{Device: "/Users/me/app/conf", Mountpoint: "/etc/app", Fstype: "virtiofs"},                          // a Mac folder shared into the VM
			{Device: "/dev/loop3", Mountpoint: "/snap/core/1", Fstype: "squashfs"},
			{Device: "/dev/sdb1", Mountpoint: "/secret", Fstype: "ext4"}, // unreadable
		},
		usage: map[string]*disk.UsageStat{
			"/":                {Total: 5000, Used: 5000, UsedPercent: 100},
			"/etc/hosts":       {Total: 1000, Used: 250, Free: 750, UsedPercent: 25},
			"/etc/resolv.conf": {Total: 1000, Used: 250, Free: 750, UsedPercent: 25},
			"/dev":             {Total: 10, Used: 10, UsedPercent: 100},
			"/snap/core/1":     {Total: 10, Used: 10, UsedPercent: 100},
			"/etc/app":         {Total: 900, Used: 9, UsedPercent: 1},
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
	// One device, once, found through its bind mount: not the overlay root,
	// not the pseudo filesystems, not the network mount (whose statfs
	// DiskUsage would have failed), not the unreadable one.
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

// A Mac: one APFS container with five volumes that each report its size,
// plus an HFS+ USB disk with two real partitions.
func TestHost_APFSVolumesAreOneDisk(t *testing.T) {
	src := machine()
	src.parts = []disk.PartitionStat{
		{Device: "/dev/disk3s1s1", Mountpoint: "/", Fstype: "apfs"},
		{Device: "/dev/disk3s5", Mountpoint: "/System/Volumes/Data", Fstype: "apfs"},
		{Device: "/dev/disk3s6", Mountpoint: "/System/Volumes/VM", Fstype: "apfs"},
		{Device: "/dev/disk3s2", Mountpoint: "/System/Volumes/Preboot", Fstype: "apfs"},
		{Device: "/dev/disk3s4", Mountpoint: "/System/Volumes/Update", Fstype: "apfs"},
		{Device: "/dev/disk5s1", Mountpoint: "/Volumes/A", Fstype: "hfs"},
		{Device: "/dev/disk5s2", Mountpoint: "/Volumes/B", Fstype: "hfs"},
	}
	container := func(used uint64) *disk.UsageStat {
		return &disk.UsageStat{Total: 1000, Free: 600, Used: used, UsedPercent: float64(used) / 10}
	}
	src.usage = map[string]*disk.UsageStat{
		"/": container(10), "/System/Volumes/Data": container(300), "/System/Volumes/VM": container(50),
		"/System/Volumes/Preboot": container(20), "/System/Volumes/Update": container(20),
		"/Volumes/A": {Total: 100, Used: 10, Free: 90, UsedPercent: 10},
		"/Volumes/B": {Total: 200, Used: 20, Free: 180, UsedPercent: 10},
	}
	g, _ := run(t, newCollector(src, testutil.NewFakeClock(t0)))
	byDev := map[string]float64{}
	for _, m := range g["system.disk.total"] {
		byDev[m.Tags[0]] += m.Value
	}
	want := map[string]float64{"device:/dev/disk3": 1000, "device:/dev/disk5s1": 100, "device:/dev/disk5s2": 200}
	if len(byDev) != len(want) {
		t.Fatalf("disk.total by device = %v, want %v", byDev, want)
	}
	for k, v := range want {
		if byDev[k] != v {
			t.Errorf("%s total = %v, want %v", k, byDev[k], v)
		}
	}
	for _, m := range g["system.disk.used"] {
		if m.Tags[0] == "device:/dev/disk3" && m.Value != 400 {
			t.Errorf("container used = %v, want 400 (total - free, not one volume's used)", m.Value)
		}
	}
	for _, m := range g["system.disk.in_use"] {
		if m.Tags[0] == "device:/dev/disk3" && m.Value != 0.4 {
			t.Errorf("container in_use = %v, want 0.4", m.Value)
		}
	}
}

// A device whose first mount cannot be read but whose second can is
// reported, and is not an error.
func TestHost_AnUnreadableMountOfAReadableDeviceIsNotAnError(t *testing.T) {
	src := machine()
	src.parts = []disk.PartitionStat{
		{Device: "/dev/vda1", Mountpoint: "/root-only", Fstype: "ext4"},
		{Device: "/dev/vda1", Mountpoint: "/etc/hosts", Fstype: "ext4"},
	}
	src.usage = map[string]*disk.UsageStat{"/etc/hosts": {Total: 10, Used: 1, Free: 9, UsedPercent: 10}}
	g, err := run(t, newCollector(src, testutil.NewFakeClock(t0)))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if m := g.one(t, "system.disk.total"); m.Tags[0] != "device:/dev/vda1" {
		t.Fatalf("%+v", m)
	}
}

func TestHost_IOIsPerWholePhysicalDisk(t *testing.T) {
	src := machine()
	fc := testutil.NewFakeClock(t0)
	c := newCollector(src, fc)
	src.io = map[string]disk.IOCountersStat{}
	for _, n := range []string{"sda", "sda1", "sda2", "nvme0n1", "nvme0n1p1", "mmcblk0", "mmcblk0p2", "vda",
		"loop0", "loop12", "ram0", "zram0", "sr0", "dm-0", "md127", "disk0"} {
		src.io[n] = disk.IOCountersStat{ReadCount: 1}
	}
	_, _ = run(t, c)
	fc.Advance(10 * time.Second)
	for n, s := range src.io {
		s.ReadCount += 10
		src.io[n] = s
	}
	g, _ := run(t, c)
	var got []string
	for _, m := range g["system.io.r_s"] {
		got = append(got, strings.TrimPrefix(m.Tags[0], "device:"))
	}
	slices.Sort(got)
	if want := []string{"disk0", "mmcblk0", "nvme0n1", "sda", "vda"}; !slices.Equal(got, want) {
		t.Fatalf("io devices = %v, want %v", got, want)
	}
}
