package process

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	gprocess "github.com/shirou/gopsutil/v4/process"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Name is the check's registered name.
const Name = "process"

// Config is one instance's settings. Exactly one of ProcessName, Pattern
// and PIDFile selects the processes.
type Config struct {
	// ProcessName matches the process name (the executable's, as ps -c
	// shows it). Not `name`: that is every instance's own name.
	ProcessName string `yaml:"process_name"`
	// ExactMatch, with ProcessName: true (the default) compares the process name
	// exactly; false looks for ProcessName anywhere in the command line.
	ExactMatch *bool `yaml:"exact_match"`
	// Pattern is a regular expression over the command line.
	Pattern string `yaml:"pattern"`
	// PIDFile is a file holding one pid, as many daemons write.
	PIDFile string `yaml:"pid_file"`
	// Label is the process_name tag's value. Default ProcessName; required with
	// Pattern or PIDFile, whose text would make a poor tag.
	Label string `yaml:"label"`
}

// Proc is a process as listed: enough to decide whether it matches.
type Proc struct {
	PID     int32
	Name    string
	Cmdline string
}

// Usage is one process's resource use. FDsOK is false when the count
// cannot be read (permissions, or a platform without it).
type Usage struct {
	CPUSeconds float64 // user + system, since the process started
	// Started identifies the process together with its pid (unix ms), so a
	// pid the kernel reuses is not differenced against its predecessor.
	Started int64
	RSS     uint64
	Threads int32
	FDs     int32
	FDsOK   bool
}

// Fields says which of a [Proc]'s texts a listing must read. Each is one
// read per process (/proc/<pid>/comm, /proc/<pid>/cmdline on Linux), over
// the whole process table, on every run of every instance — so an instance
// asks only for the one its match compares: an exact process_name needs
// the name, a pattern or a substring match the command line.
type Fields uint8

// The fields a listing can read.
const (
	WantName Fields = 1 << iota
	WantCmdline
)

// Source is what the check reads; [System] is the real one.
type Source interface {
	// List returns every process, with the texts want asks for (the others
	// are left empty). Name and Cmdline may also be empty for a process
	// that exited mid-listing or cannot be read.
	List(ctx context.Context, want Fields) ([]Proc, error)
	// Usage reads one process. An error means it is gone and is skipped —
	// unless it is a permission error (errors.Is fs.ErrPermission): the
	// process is there, the agent may not read it.
	Usage(ctx context.Context, pid int32) (Usage, error)
}

// Check is one process instance.
type Check struct {
	cfg     Config
	exact   bool
	want    Fields // what match compares, so all List reads
	pattern *regexp.Regexp
	src     Source
	clock   clock.Clock
	rates   *collector.Rates
	tags    []string
}

var _ collector.Collector = (*Check)(nil)

// New is the process factory.
func New(inst collector.Instance) (collector.Collector, error) {
	var cfg Config
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	return build(cfg, System{}, inst.Clock)
}

func build(cfg Config, src Source, clk clock.Clock) (*Check, error) {
	set := 0
	for _, s := range []string{cfg.ProcessName, cfg.Pattern, cfg.PIDFile} {
		if s != "" {
			set++
		}
	}
	if set != 1 {
		return nil, errors.New("set exactly one of process_name, pattern and pid_file")
	}
	c := &Check{cfg: cfg, exact: cfg.ExactMatch == nil || *cfg.ExactMatch, src: src, clock: clk, rates: collector.NewRates()}
	if cfg.ExactMatch != nil && cfg.ProcessName == "" {
		return nil, errors.New("exact_match applies only to process_name")
	}
	if cfg.Pattern != "" {
		rx, err := regexp.Compile(cfg.Pattern)
		if err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
		c.pattern = rx
	}
	label := cfg.Label
	if label == "" {
		if cfg.ProcessName == "" {
			return nil, errors.New("label is required with pattern or pid_file")
		}
		label = cfg.ProcessName
	}
	tag, ok := wire.NormalizeTag("process_name:" + label)
	if !ok {
		return nil, fmt.Errorf("process_name:%s cannot be a tag; set label", label)
	}
	c.tags = []string{tag}
	c.want = WantCmdline
	if c.pattern == nil && c.exact {
		c.want = WantName
	}
	if c.clock == nil {
		c.clock = clock.Real()
	}
	return c, nil
}

// Name implements [collector.Collector]; the instance wrapper names it.
func (c *Check) Name() string { return Name }

// Interval implements [collector.Collector]: the scheduler's default.
func (c *Check) Interval() time.Duration { return 0 }

// Collect finds the matching processes and reports on them.
func (c *Check) Collect(ctx context.Context, emit collector.Emit) error {
	now := c.clock.Now()
	defer c.rates.Sweep(now)
	pids, err := c.match(ctx)
	if err != nil {
		return err
	}
	var (
		n, denied    int
		rss          uint64
		threads, fds int64
		fdsOK        = true
		cpu          float64
		cpuOK        bool
	)
	for _, pid := range pids {
		u, err := c.src.Usage(ctx, pid)
		if err != nil {
			// A process the agent may not read (another user's, run
			// natively without root) is running all the same: counting it
			// as gone would chart an outage of a process that is up.
			if isPermission(err) {
				n++
				denied++
			}
			continue // otherwise exited since the listing, or the run was cut off
		}
		n++
		rss += u.RSS
		threads += int64(u.Threads)
		fds += int64(u.FDs)
		fdsOK = fdsOK && u.FDsOK
		key := strconv.Itoa(int(pid)) + "@" + strconv.FormatInt(u.Started, 10)
		if r, ok := c.rates.Observe(key, u.CPUSeconds, now); ok {
			cpu += r * 100
			cpuOK = true
		}
	}
	// A Usage that failed because the run was cut off (its timeout, or
	// shutdown) is not a process that exited: reading the process table
	// is context-aware on some platforms, so every call fails at once and
	// n would count none. Reporting that would read as "the process is
	// down" with no error to explain it. Report nothing and say why.
	if err := ctx.Err(); err != nil {
		return err
	}
	gauge := func(name string, v float64) {
		emit(collector.Metric{Name: name, Kind: collector.Gauge, Value: v, Tags: c.tags})
	}
	gauge("system.processes.number", float64(n))
	if n == denied {
		// Nothing was readable: a zero rss or thread count would be a
		// reading of nothing, not of the processes.
		return deniedErr(denied)
	}
	gauge("system.processes.mem.rss", float64(rss))
	gauge("system.processes.threads", float64(threads))
	if fdsOK {
		gauge("system.processes.open_file_descriptors", float64(fds))
	}
	if cpuOK {
		gauge("system.processes.cpu.pct", cpu)
	}
	return deniedErr(denied)
}

// isPermission reports whether a Usage error means the process exists but
// cannot be read. gopsutil returns the system call's errno (EPERM, EACCES)
// or an *fs.PathError around it, both of which match fs.ErrPermission.
func isPermission(err error) bool { return errors.Is(err, fs.ErrPermission) }

// deniedErr is the run's error when some matching processes could not be
// read: they are counted, but their cpu and memory are not, and the sums
// would otherwise undercount without a word. Returned, the scheduler counts
// it and logs it once, on the transition, rather than every run.
func deniedErr(denied int) error {
	if denied == 0 {
		return nil
	}
	return fmt.Errorf("%d matching process(es) could not be read (permission denied): counted, but their cpu, memory, threads and file descriptors are not; run the agent with access to them", denied)
}

// match returns the pids the instance describes.
func (c *Check) match(ctx context.Context) ([]int32, error) {
	if c.cfg.PIDFile != "" {
		b, err := os.ReadFile(c.cfg.PIDFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // not running: the daemon removes its pid file
		}
		if err != nil {
			return nil, err
		}
		pid, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 32)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("pid_file %s: %q is not a pid", c.cfg.PIDFile, strings.TrimSpace(string(b)))
		}
		return []int32{int32(pid)}, nil
	}
	procs, err := c.src.List(ctx, c.want)
	if err != nil {
		return nil, err
	}
	var out []int32
	for _, p := range procs {
		var ok bool
		switch {
		case c.pattern != nil:
			ok = c.pattern.MatchString(p.Cmdline)
		case c.exact:
			ok = p.Name == c.cfg.ProcessName
		default:
			ok = strings.Contains(p.Cmdline, c.cfg.ProcessName)
		}
		if ok {
			out = append(out, p.PID)
		}
	}
	return out, nil
}

// System reads the real process table through gopsutil.
type System struct{}

// List implements [Source].
func (System) List(ctx context.Context, want Fields) ([]Proc, error) {
	ps, err := gprocess.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(ps))
	for _, p := range ps {
		proc := Proc{PID: p.Pid}
		if want&WantName != 0 {
			proc.Name, _ = p.NameWithContext(ctx)
		}
		if want&WantCmdline != 0 {
			proc.Cmdline, _ = p.CmdlineWithContext(ctx)
		}
		out = append(out, proc)
	}
	return out, nil
}

// Usage implements [Source].
func (System) Usage(ctx context.Context, pid int32) (Usage, error) {
	p, err := gprocess.NewProcessWithContext(ctx, pid)
	if err != nil {
		return Usage{}, err
	}
	t, err := p.TimesWithContext(ctx)
	if err != nil {
		return Usage{}, err
	}
	m, err := p.MemoryInfoWithContext(ctx)
	if err != nil {
		return Usage{}, err
	}
	u := Usage{CPUSeconds: t.User + t.System, RSS: m.RSS}
	u.Started, _ = p.CreateTimeWithContext(ctx)
	u.Threads, _ = p.NumThreadsWithContext(ctx)
	if n, err := p.NumFDsWithContext(ctx); err == nil {
		u.FDs, u.FDsOK = n, true
	}
	return u, nil
}
