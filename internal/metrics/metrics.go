// Package metrics polls LXD's built-in Prometheus-format metrics endpoint
// (`lxc query /1.0/metrics`) and keeps a small in-memory rolling history of
// per-container CPU, memory and network rates for the dashboard's monitoring
// charts. No extra daemons or third-party agents — purely the LXD API LXD
// already exposes.
package metrics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommandRunner executes an lxc command and returns its combined output.
// Mirrors internal/lxdctl.CommandRunner so the same shell-out pattern (and
// test-mocking style) is used throughout the app.
type CommandRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Options configures a Collector.
type Options struct {
	// Bin is the lxc binary (default "lxc").
	Bin string
	// Sudo runs lxc via `sudo -n`.
	Sudo bool
	// Interval between polls (default 10s).
	Interval time.Duration
	// Window caps how many samples each series retains (default 360 — at a
	// 10s interval that's an hour of history, a few KB per container).
	Window int
	// Runner overrides command execution (used in tests).
	Runner CommandRunner
}

// Point is one timestamped sample, JSON-friendly for the chart endpoint.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Snapshot is the JSON view of one container's tracked series, mirroring the
// CPU/network panels of a typical cloud-provider monitoring tab.
type Snapshot struct {
	CPUPercent         []Point `json:"cpuPercent"`
	MemoryPercent      []Point `json:"memoryPercent"`
	NetRxBytesPerSec   []Point `json:"netRxBytesPerSec"`
	NetTxBytesPerSec   []Point `json:"netTxBytesPerSec"`
	NetRxPacketsPerSec []Point `json:"netRxPacketsPerSec"`
	NetTxPacketsPerSec []Point `json:"netTxPacketsPerSec"`
}

// series is a fixed-size ring buffer of samples for one chart line.
type series struct {
	mu     sync.Mutex
	points []Point
	max    int
}

func newSeries(max int) *series { return &series{max: max} }

func (s *series) add(at time.Time, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.points = append(s.points, Point{At: at, Value: v})
	if len(s.points) > s.max {
		s.points = s.points[len(s.points)-s.max:]
	}
}

func (s *series) snapshot() []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Point, len(s.points))
	copy(out, s.points)
	return out
}

// tracked is the set of derived series kept for one container.
type tracked struct {
	cpuPercent    *series
	memoryPercent *series
	netRxBytes    *series
	netTxBytes    *series
	netRxPackets  *series
	netTxPackets  *series
}

func newTracked(window int) *tracked {
	return &tracked{
		cpuPercent:    newSeries(window),
		memoryPercent: newSeries(window),
		netRxBytes:    newSeries(window),
		netTxBytes:    newSeries(window),
		netRxPackets:  newSeries(window),
		netTxPackets:  newSeries(window),
	}
}

// rawSample is one poll's cumulative counters/gauges for a container, parsed
// straight from the Prometheus text. Rates are derived from the delta between
// consecutive samples.
type rawSample struct {
	at           time.Time
	cpuSeconds   float64
	cpuCount     float64
	memTotal     float64
	memAvailable float64
	netRxBytes   float64
	netTxBytes   float64
	netRxPackets float64
	netTxPackets float64
}

// Collector periodically polls LXD's metrics endpoint and keeps a rolling
// history of derived per-container rates.
type Collector struct {
	runner   CommandRunner
	interval time.Duration
	window   int

	mu   sync.RWMutex
	data map[string]*tracked
	prev map[string]rawSample
}

// NewCollector builds a Collector from Options, applying sensible defaults.
func NewCollector(opts Options) *Collector {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Window <= 0 {
		opts.Window = 360
	}
	runner := opts.Runner
	if runner == nil {
		bin := opts.Bin
		if bin == "" {
			bin = "lxc"
		}
		runner = &cliRunner{bin: bin, sudo: opts.Sudo}
	}
	return &Collector{
		runner:   runner,
		interval: opts.Interval,
		window:   opts.Window,
		data:     make(map[string]*tracked),
		prev:     make(map[string]rawSample),
	}
}

// Run polls on Interval until ctx is cancelled. Intended to run in its own
// goroutine for the lifetime of the server.
func (c *Collector) Run(ctx context.Context) {
	c.poll(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Collector) poll(ctx context.Context) {
	out, err := c.runner.Run(ctx, "query", "/1.0/metrics")
	if err != nil {
		return // transient — try again next tick
	}
	now := time.Now()
	for name, cur := range parseMetrics(out) {
		cur.at = now
		c.record(name, cur)
	}
}

func (c *Collector) record(name string, cur rawSample) {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev, hadPrev := c.prev[name]
	c.prev[name] = cur
	if !hadPrev {
		return // need two samples to derive a rate
	}
	dt := cur.at.Sub(prev.at).Seconds()
	if dt <= 0 {
		return
	}

	t, ok := c.data[name]
	if !ok {
		t = newTracked(c.window)
		c.data[name] = t
	}
	if cur.cpuCount > 0 {
		t.cpuPercent.add(cur.at, clampPercent(rate(cur.cpuSeconds, prev.cpuSeconds, dt)/cur.cpuCount*100))
	}
	if cur.memTotal > 0 {
		used := cur.memTotal - cur.memAvailable
		t.memoryPercent.add(cur.at, clampPercent(used/cur.memTotal*100))
	}
	t.netRxBytes.add(cur.at, rate(cur.netRxBytes, prev.netRxBytes, dt))
	t.netTxBytes.add(cur.at, rate(cur.netTxBytes, prev.netTxBytes, dt))
	t.netRxPackets.add(cur.at, rate(cur.netRxPackets, prev.netRxPackets, dt))
	t.netTxPackets.add(cur.at, rate(cur.netTxPackets, prev.netTxPackets, dt))
}

// Snapshot returns the current series for name, or false if nothing has been
// collected for it yet (e.g. it was created less than two poll intervals ago).
func (c *Collector) Snapshot(name string) (Snapshot, bool) {
	c.mu.RLock()
	t, ok := c.data[name]
	c.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	return Snapshot{
		CPUPercent:         t.cpuPercent.snapshot(),
		MemoryPercent:      t.memoryPercent.snapshot(),
		NetRxBytesPerSec:   t.netRxBytes.snapshot(),
		NetTxBytesPerSec:   t.netTxBytes.snapshot(),
		NetRxPacketsPerSec: t.netRxPackets.snapshot(),
		NetTxPacketsPerSec: t.netTxPackets.snapshot(),
	}, true
}

// rate returns the non-negative per-second delta between two cumulative
// counter readings. Counters reset to zero on container restart, in which
// case we report 0 for that interval rather than a bogus negative spike.
func rate(cur, prev, dt float64) float64 {
	if dt <= 0 || cur < prev {
		return 0
	}
	return (cur - prev) / dt
}

func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// metricLineRE matches Prometheus exposition lines: `name{labels} value`.
var metricLineRE = regexp.MustCompile(`^(lxd_[a-zA-Z_]+)\{([^}]*)\}\s+([0-9eE.+-]+)\s*$`)

// labelRE matches individual `key="value"` label pairs.
var labelRE = regexp.MustCompile(`(\w+)="([^"]*)"`)

// parseMetrics extracts the per-container counters/gauges this package tracks
// from LXD's Prometheus-format /1.0/metrics output. Only `type="container"`
// series are kept, and network counters are taken from the `eth0` device only
// (the container's primary NIC) to mirror how a cloud console reports a
// single instance's network in/out rather than summing every internal veth.
func parseMetrics(data []byte) map[string]rawSample {
	out := make(map[string]rawSample)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		m := metricLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		metric, labelStr, valStr := m[1], m[2], m[3]
		labels := parseLabels(labelStr)
		if labels["type"] != "container" {
			continue
		}
		name := labels["name"]
		if name == "" {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		s := out[name]
		switch metric {
		case "lxd_cpu_seconds_total":
			s.cpuSeconds += val
		case "lxd_cpu_effective_total":
			s.cpuCount = val
		case "lxd_memory_MemTotal_bytes":
			s.memTotal = val
		case "lxd_memory_MemAvailable_bytes":
			s.memAvailable = val
		case "lxd_network_receive_bytes_total":
			if labels["device"] == "eth0" {
				s.netRxBytes = val
			}
		case "lxd_network_transmit_bytes_total":
			if labels["device"] == "eth0" {
				s.netTxBytes = val
			}
		case "lxd_network_receive_packets_total":
			if labels["device"] == "eth0" {
				s.netRxPackets = val
			}
		case "lxd_network_transmit_packets_total":
			if labels["device"] == "eth0" {
				s.netTxPackets = val
			}
		}
		out[name] = s
	}
	return out
}

func parseLabels(s string) map[string]string {
	out := make(map[string]string, 4)
	for _, m := range labelRE.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// cliRunner is the production CommandRunner using the lxc CLI — identical in
// shape to internal/lxdctl's execRunner so behavior (sudo handling, error
// formatting) stays consistent across the app.
type cliRunner struct {
	bin  string
	sudo bool
}

func (r *cliRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	name := r.bin
	full := args
	if r.sudo {
		name = "sudo"
		full = append([]string{"-n", r.bin}, args...)
	}
	cmd := exec.CommandContext(ctx, name, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
