package host

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/clock"
)

// Source is where the collector reads the machine from. It is gopsutil's
// API, narrowed to what is used, so tests can supply canned readings (and
// readings no real machine will produce on demand: a counter reset, an
// interface appearing, a partition that cannot be read). A reading the
// platform cannot provide is an error wrapping [errors.ErrUnsupported].
type Source interface {
	CPUTimes(ctx context.Context) (cpu.TimesStat, error)
	Load(ctx context.Context) (*load.AvgStat, error)
	VirtualMemory(ctx context.Context) (*mem.VirtualMemoryStat, error)
	SwapMemory(ctx context.Context) (*mem.SwapMemoryStat, error)
	Partitions(ctx context.Context) ([]disk.PartitionStat, error)
	DiskUsage(ctx context.Context, path string) (*disk.UsageStat, error)
	DiskIO(ctx context.Context) (map[string]disk.IOCountersStat, error)
	NetIO(ctx context.Context) ([]net.IOCountersStat, error)
	Uptime(ctx context.Context) (uint64, error)
}

// Options configures the host collector.
type Options struct {
	// Interval is how often to run; zero means the scheduler's default.
	Interval time.Duration
	// ExcludeInterfaces skips network interfaces whose name matches any of
	// these (the agent's default is config.DefaultExcludeInterfaces).
	ExcludeInterfaces []*regexp.Regexp
	// Source defaults to the real machine (gopsutil).
	Source Source
	Clock  clock.Clock // default clock.Real()
}

// pseudoFS are filesystem types with no disk behind them, which physical
// partition listing still returns on some platforms (devfs on macOS).
var pseudoFS = map[string]bool{
	"devfs": true, "autofs": true, "devtmpfs": true, "tmpfs": true, "proc": true,
	"sysfs": true, "cgroup": true, "cgroup2": true, "nsfs": true, "squashfs": true,
}

// Collector reports the machine the agent runs on: CPU, load, memory, swap,
// disks, disk I/O, network and uptime (docs/metrics-catalog.md, system.*).
type Collector struct {
	iv      time.Duration
	exclude []*regexp.Regexp
	src     Source
	clock   clock.Clock
	rates   *collector.Rates
	// prevCPU is the last CPU reading; percentages need two.
	prevCPU *cpu.TimesStat
}

var _ collector.Collector = (*Collector)(nil)

// New returns a host collector.
func New(opts Options) *Collector {
	if opts.Source == nil {
		opts.Source = gopsutil{}
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	return &Collector{iv: opts.Interval, exclude: opts.ExcludeInterfaces, src: opts.Source, clock: opts.Clock, rates: collector.NewRates()}
}

// Name implements [collector.Collector].
func (c *Collector) Name() string { return "host" }

// Interval implements [collector.Collector].
func (c *Collector) Interval() time.Duration { return c.iv }

// forgetAfter is how long a device or interface's last counter reading is
// kept once it stops appearing. Long enough to survive a few failed runs,
// short enough that a laptop's churn of tunnels and USB disks does not
// accumulate.
const forgetAfter = 5 * time.Minute

// Collect reads every group. A group that fails does not stop the others:
// its error joins the returned one and the rest are still emitted. A group
// the platform does not support ([errors.ErrUnsupported], e.g. disk I/O in
// a cgo-less macOS build) is skipped silently — it is not a failure of
// this run, and would otherwise be "logged once" forever.
func (c *Collector) Collect(ctx context.Context, emit collector.Emit) error {
	now := c.clock.Now()
	defer c.rates.Prune(now.Add(-forgetAfter))
	var errs []error
	for _, g := range []struct {
		name string
		fn   func(context.Context, collector.Emit, time.Time) error
	}{
		{"cpu", c.cpu}, {"load", c.load}, {"memory", c.memory}, {"swap", c.swap},
		{"disk", c.disk}, {"io", c.io}, {"net", c.net}, {"uptime", c.uptime},
	} {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := g.fn(ctx, emit, now); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			errs = append(errs, fmt.Errorf("%s: %w", g.name, err))
		}
	}
	return errors.Join(errs...)
}

func gauge(emit collector.Emit, name string, v float64, tags ...string) {
	emit(collector.Metric{Name: name, Kind: collector.Gauge, Value: v, Tags: tags})
}

// total is all the CPU time the kernel accounted, the denominator of every
// percentage. It is summed here rather than taken from gopsutil's
// TimesStat.Total, which adds
// Guest and GuestNice: on Linux those are already inside User and Nice
// (/proc/stat counts a guest's time twice, once as guest and once as user),
// so Total over-counts on any host running VMs and every percentage comes out
// low.
func total(t cpu.TimesStat) float64 {
	return t.User + t.Nice + t.System + t.Idle + t.Iowait + t.Irq + t.Softirq + t.Steal
}

// cpu reports how the machine's CPU time was spent since the previous run,
// as percentages of all cores together (0–100, not 0–100×cores). The kernel
// only offers cumulative seconds per state, so a percentage needs two
// readings: the first run emits nothing. user includes nice (both are time
// running user code; nice only changed its priority), and system includes
// interrupt handling (irq, softirq), so the five add up to 100.
func (c *Collector) cpu(ctx context.Context, emit collector.Emit, _ time.Time) error {
	cur, err := c.src.CPUTimes(ctx)
	if err != nil {
		return err
	}
	prev := c.prevCPU
	c.prevCPU = &cur
	if prev == nil {
		return nil
	}
	d := total(cur) - total(*prev)
	// Zero: no time passed as far as the kernel is concerned. Negative: the
	// counters went back (a reboot under a long-lived agent is impossible,
	// but a VM restore or a hot-unplugged core is not). Either way this
	// interval has no answer.
	if d <= 0 {
		return nil
	}
	pct := func(cur, prev float64) float64 { return max(0, (cur-prev)/d*100) }
	gauge(emit, "system.cpu.user", pct(cur.User+cur.Nice, prev.User+prev.Nice))
	gauge(emit, "system.cpu.system", pct(cur.System+cur.Irq+cur.Softirq, prev.System+prev.Irq+prev.Softirq))
	gauge(emit, "system.cpu.idle", pct(cur.Idle, prev.Idle))
	gauge(emit, "system.cpu.iowait", pct(cur.Iowait, prev.Iowait))
	gauge(emit, "system.cpu.stolen", pct(cur.Steal, prev.Steal))
	return nil
}

func (c *Collector) load(ctx context.Context, emit collector.Emit, _ time.Time) error {
	l, err := c.src.Load(ctx)
	if err != nil {
		return err
	}
	gauge(emit, "system.load.1", l.Load1)
	gauge(emit, "system.load.5", l.Load5)
	gauge(emit, "system.load.15", l.Load15)
	return nil
}

// memory reports bytes. usable is what the kernel says it could give a new
// process without swapping (MemAvailable on Linux: free plus reclaimable
// cache), which is the number that answers "is this machine short of
// memory" — free alone is near zero on any healthy Linux box, because unused
// memory is used as cache.
func (c *Collector) memory(ctx context.Context, emit collector.Emit, _ time.Time) error {
	m, err := c.src.VirtualMemory(ctx)
	if err != nil {
		return err
	}
	gauge(emit, "system.mem.total", float64(m.Total))
	gauge(emit, "system.mem.used", float64(m.Used))
	gauge(emit, "system.mem.free", float64(m.Free))
	gauge(emit, "system.mem.usable", float64(m.Available))
	if m.Total > 0 {
		gauge(emit, "system.mem.pct_usable", float64(m.Available)/float64(m.Total))
	}
	return nil
}

func (c *Collector) swap(ctx context.Context, emit collector.Emit, _ time.Time) error {
	s, err := c.src.SwapMemory(ctx)
	if err != nil {
		return err
	}
	gauge(emit, "system.swap.total", float64(s.Total))
	gauge(emit, "system.swap.used", float64(s.Used))
	gauge(emit, "system.swap.free", float64(s.Free))
	if s.Total > 0 {
		gauge(emit, "system.swap.pct_free", float64(s.Free)/float64(s.Total))
	}
	return nil
}

// disk reports space per device. A device mounted in several places (bind
// mounts, macOS's firmlinked volumes, a container's view of the host disk)
// is reported once, at its first mount point, so its bytes are not counted
// twice by a sum over devices. A partition that cannot be read (permission,
// a stale network mount) is skipped, and the error returned after the rest.
func (c *Collector) disk(ctx context.Context, emit collector.Emit, _ time.Time) error {
	parts, err := c.src.Partitions(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var errs []error
	for _, p := range parts {
		if p.Device == "" || seen[p.Device] || pseudoFS[p.Fstype] {
			continue
		}
		u, err := c.src.DiskUsage(ctx, p.Mountpoint)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Mountpoint, err))
			continue
		}
		if u.Total == 0 {
			continue // pseudo filesystems (devfs, autofs) have no size
		}
		seen[p.Device] = true
		tag := "device:" + p.Device
		gauge(emit, "system.disk.total", float64(u.Total), tag)
		gauge(emit, "system.disk.used", float64(u.Used), tag)
		gauge(emit, "system.disk.free", float64(u.Free), tag)
		gauge(emit, "system.disk.in_use", u.UsedPercent/100, tag)
	}
	return errors.Join(errs...)
}

// rate emits the per-second rate of a cumulative counter, if there is one.
func (c *Collector) rate(emit collector.Emit, key, name string, v float64, at time.Time, tag string) {
	if r, ok := c.rates.Observe(key+"\x00"+name, v, at); ok {
		emit(collector.Metric{Name: name, Kind: collector.Rate, Value: r, Tags: []string{tag}})
	}
}

// io reports disk operations and kilobytes per second, per device.
func (c *Collector) io(ctx context.Context, emit collector.Emit, now time.Time) error {
	counters, err := c.src.DiskIO(ctx)
	if err != nil {
		return err
	}
	for dev, s := range counters {
		tag, key := "device:"+dev, "io:"+dev
		c.rate(emit, key, "system.io.r_s", float64(s.ReadCount), now, tag)
		c.rate(emit, key, "system.io.w_s", float64(s.WriteCount), now, tag)
		c.rate(emit, key, "system.io.rkb_s", float64(s.ReadBytes)/1024, now, tag)
		c.rate(emit, key, "system.io.wkb_s", float64(s.WriteBytes)/1024, now, tag)
	}
	return nil
}

// net reports traffic per interface. An interface that has never moved a
// packet is skipped, and so is one matching ExcludeInterfaces: eight series
// each, and a Mac has a dozen virtual ones.
func (c *Collector) net(ctx context.Context, emit collector.Emit, now time.Time) error {
	ifs, err := c.src.NetIO(ctx)
	if err != nil {
		return err
	}
	for _, s := range ifs {
		if s.PacketsRecv == 0 && s.PacketsSent == 0 || c.excluded(s.Name) {
			continue
		}
		tag, key := "interface:"+s.Name, "net:"+s.Name
		for _, m := range []struct {
			name string
			v    uint64
		}{
			{"system.net.bytes_rcvd", s.BytesRecv},
			{"system.net.bytes_sent", s.BytesSent},
			{"system.net.packets_in.count", s.PacketsRecv},
			{"system.net.packets_out.count", s.PacketsSent},
			{"system.net.packets_in.error", s.Errin},
			{"system.net.packets_out.error", s.Errout},
			{"system.net.packets_in.drop", s.Dropin},
			{"system.net.packets_out.drop", s.Dropout},
		} {
			c.rate(emit, key, m.name, float64(m.v), now, tag)
		}
	}
	return nil
}

func (c *Collector) excluded(name string) bool {
	for _, re := range c.exclude {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

func (c *Collector) uptime(ctx context.Context, emit collector.Emit, _ time.Time) error {
	u, err := c.src.Uptime(ctx)
	if err != nil {
		return err
	}
	gauge(emit, "system.uptime", float64(u))
	return nil
}

// gopsutil is the real Source.
type gopsutil struct{}

// notImplemented is gopsutil's "not implemented yet" error. It lives in an
// internal package, so it cannot be matched with errors.Is; its text is the
// only handle there is.
const notImplemented = "not implemented yet"

// supported translates gopsutil's "this platform can't" into the standard
// library's, so the collector does not depend on an error string.
func supported[T any](v T, err error) (T, error) {
	if err != nil && err.Error() == notImplemented {
		return v, fmt.Errorf("%w: %w", errors.ErrUnsupported, err)
	}
	return v, err
}

func (gopsutil) CPUTimes(ctx context.Context) (cpu.TimesStat, error) {
	ts, err := supported(cpu.TimesWithContext(ctx, false))
	if err != nil {
		return cpu.TimesStat{}, err
	}
	if len(ts) == 0 {
		return cpu.TimesStat{}, errors.New("no CPU times reported")
	}
	return ts[0], nil
}

func (gopsutil) Load(ctx context.Context) (*load.AvgStat, error) {
	return supported(load.AvgWithContext(ctx))
}

func (gopsutil) VirtualMemory(ctx context.Context) (*mem.VirtualMemoryStat, error) {
	return supported(mem.VirtualMemoryWithContext(ctx))
}

func (gopsutil) SwapMemory(ctx context.Context) (*mem.SwapMemoryStat, error) {
	return supported(mem.SwapMemoryWithContext(ctx))
}

// Partitions lists physical partitions only (all=false): no proc, sysfs,
// tmpfs and the like.
func (gopsutil) Partitions(ctx context.Context) ([]disk.PartitionStat, error) {
	return supported(disk.PartitionsWithContext(ctx, false))
}

func (gopsutil) DiskUsage(ctx context.Context, path string) (*disk.UsageStat, error) {
	return supported(disk.UsageWithContext(ctx, path))
}

func (gopsutil) DiskIO(ctx context.Context) (map[string]disk.IOCountersStat, error) {
	return supported(disk.IOCountersWithContext(ctx))
}

func (gopsutil) NetIO(ctx context.Context) ([]net.IOCountersStat, error) {
	return supported(net.IOCountersWithContext(ctx, true))
}

func (gopsutil) Uptime(ctx context.Context) (uint64, error) {
	return supported(host.UptimeWithContext(ctx))
}
