// Package metrics collects host and container utilization snapshots from LXD and
// Linux /proc files. The UI renders this data as a live table, not charts.
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommandRunner executes an lxc command and returns combined output.
type CommandRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Options configures a Collector.
type Options struct {
	// Bin is the lxc binary (default "lxc").
	Bin string
	// Sudo runs lxc via `sudo -n`.
	Sudo bool
	// Interval between polls (default 5s).
	Interval time.Duration
	// Runner overrides command execution (used in tests).
	Runner CommandRunner
	// ReadFile overrides /proc reads (used in tests).
	ReadFile func(name string) ([]byte, error)
	// ListFn returns container rows and status (used for visibility of stopped
	// containers.
	ListFn func(ctx context.Context) ([]ContainerInfo, error)
	// Throughput ceilings in bytes/s for utilization percentages.
	DiskThroughputCapBytes    float64
	NetworkThroughputCapBytes float64
}

// ContainerInfo carries container metadata for visibility.
type ContainerInfo struct {
	Name   string
	Status string
}

// UtilizationRow is one monitoring table row.
type UtilizationRow struct {
	Name                 string   `json:"name"`
	Type                 string   `json:"type"`
	Status               string   `json:"status"`
	CPUUtilization       *float64 `json:"cpuUtilization"`
	MemoryUtilization    *float64 `json:"memoryUtilization"`
	MemoryTotalBytes     *uint64  `json:"memoryTotalBytes"`
	MemoryFreeBytes      *uint64  `json:"memoryFreeBytes"`
	IOUtilization        *float64 `json:"ioUtilization"`
	IOReadBytesPerSec    *float64 `json:"ioReadBytesPerSec"`
	IOWriteBytesPerSec   *float64 `json:"ioWriteBytesPerSec"`
	NetworkUtilization   *float64 `json:"networkUtilization"`
	NetworkRxBytesPerSec *float64 `json:"networkRxBytesPerSec"`
	NetworkTxBytesPerSec *float64 `json:"networkTxBytesPerSec"`
}

// MonitoringSnapshot is the JSON payload served by /monitoring/data.
type MonitoringSnapshot struct {
	Ready bool             `json:"ready"`
	Rows  []UtilizationRow `json:"rows"`
}

const (
	defaultHostName          = "host"
	defaultHostType          = "Host"
	defaultContainerType     = "Container"
	defaultThroughputCeiling = 100 * 1024 * 1024 // 100 MiB/s
)

type rawContainerSample struct {
	at             time.Time
	cpuSeconds     float64
	cpuCount       float64
	memTotal       float64
	memAvailable   float64
	diskReadBytes  float64
	diskWriteBytes float64
	netRxBytes     float64
	netTxBytes     float64
}

type rawHostSample struct {
	at        time.Time
	cpuTotal  float64
	cpuIdle   float64
	memTotal  float64
	memAvail  float64
	diskRead  float64
	diskWrite float64
	netRx     float64
	netTx     float64
}

// Collector polls host and container metrics and computes latest utilization.
type Collector struct {
	runner   CommandRunner
	readFile func(name string) ([]byte, error)
	listFn   func(context.Context) ([]ContainerInfo, error)
	interval time.Duration
	diskCap  float64
	netCap   float64

	mu        sync.RWMutex
	container map[string]rawContainerSample
	hostPrev  rawHostSample
	hasHost   bool
	rows      map[string]UtilizationRow
}

// NewCollector builds a Collector from Options, applying defaults.
func NewCollector(opts Options) *Collector {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	diskCap := opts.DiskThroughputCapBytes
	if diskCap <= 0 {
		diskCap = defaultThroughputCeiling
	}
	netCap := opts.NetworkThroughputCapBytes
	if netCap <= 0 {
		netCap = defaultThroughputCeiling
	}
	runner := opts.Runner
	if runner == nil {
		bin := opts.Bin
		if bin == "" {
			bin = "lxc"
		}
		runner = &cliRunner{bin: bin, sudo: opts.Sudo}
	}
	readFile := opts.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	return &Collector{
		runner:    runner,
		readFile:  readFile,
		listFn:    opts.ListFn,
		interval:  opts.Interval,
		diskCap:   diskCap,
		netCap:    netCap,
		container: make(map[string]rawContainerSample),
		rows:      make(map[string]UtilizationRow),
	}
}

// Run polls until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	if c == nil {
		return
	}
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
	if c == nil {
		return
	}
	containers, err := c.containers(ctx)
	if err != nil {
		return // keep previous rows rather than dropping visibility
	}

	now := time.Now()

	nextRows := map[string]UtilizationRow{
		defaultHostName: {
			Name:   defaultHostName,
			Type:   defaultHostType,
			Status: "Running",
		},
	}

	host, err := c.sampleHost()
	if err == nil && !isZeroHostSample(host) {
		prev := c.hostPrev
		c.hostPrev = host
		c.hostPrev.at = now
		row := nextRows[defaultHostName]
		if c.hasHost {
			c.computeHostUsage(&row, prev, host, now.Sub(prev.at).Seconds())
			nextRows[defaultHostName] = row
		} else {
			c.hasHost = true
		}
	}

	samples, err := c.sampleContainers(ctx)
	if err != nil {
		samples = make(map[string]rawContainerSample)
	}
	nextContainerSamples := make(map[string]rawContainerSample, len(containers))
	for _, container := range containers {
		row := UtilizationRow{
			Name:   container.Name,
			Type:   defaultContainerType,
			Status: container.Status,
		}
		raw, ok := samples[container.Name]
		if ok {
			raw.at = now
			prev, hadPrev := c.container[container.Name]
			if hadPrev && strings.EqualFold(container.Status, "Running") {
				c.computeContainerUsage(&row, prev, raw, now.Sub(prev.at).Seconds())
			}
			nextContainerSamples[container.Name] = raw
		}
		nextRows[container.Name] = row
	}

	c.mu.Lock()
	c.rows = nextRows
	c.container = nextContainerSamples
	c.mu.Unlock()
}

func (c *Collector) containers(ctx context.Context) ([]ContainerInfo, error) {
	if c.listFn == nil {
		return nil, nil
	}
	return c.listFn(ctx)
}

func (c *Collector) sampleHost() (rawHostSample, error) {
	now := time.Now()
	var out rawHostSample
	out.at = now

	rawStat, err := c.readFile("/proc/stat")
	if err != nil {
		return rawHostSample{}, err
	}
	total, idle, err := parseProcStat(rawStat)
	if err != nil {
		return rawHostSample{}, err
	}
	out.cpuTotal = total
	out.cpuIdle = idle

	rawMem, err := c.readFile("/proc/meminfo")
	if err != nil {
		return rawHostSample{}, err
	}
	memTotal, memAvail, err := parseProcMeminfo(rawMem)
	if err != nil {
		return rawHostSample{}, err
	}
	out.memTotal = memTotal
	out.memAvail = memAvail

	rawDisk, err := c.readFile("/proc/diskstats")
	if err != nil {
		return rawHostSample{}, err
	}
	read, write, err := parseProcDiskstats(rawDisk)
	if err != nil {
		return rawHostSample{}, err
	}
	out.diskRead = read
	out.diskWrite = write

	rawNet, err := c.readFile("/proc/net/dev")
	if err != nil {
		return rawHostSample{}, err
	}
	rx, tx, err := parseProcNetDev(rawNet)
	if err != nil {
		return rawHostSample{}, err
	}
	out.netRx = rx
	out.netTx = tx

	return out, nil
}

func (c *Collector) sampleContainers(ctx context.Context) (map[string]rawContainerSample, error) {
	out, err := c.runner.Run(ctx, "query", "/1.0/metrics")
	if err != nil {
		return map[string]rawContainerSample{}, err
	}
	return parseMetrics(out), nil
}

func (c *Collector) computeHostUsage(row *UtilizationRow, prev, cur rawHostSample, dt float64) {
	if dt <= 0 {
		return
	}
	row.CPUUtilization = rateToPercent(cur.cpuTotal-prev.cpuTotal, cur.cpuIdle-prev.cpuIdle, dt)
	if cur.memTotal > 0 {
		row.MemoryUtilization = floatPtr(clampPercent((cur.memTotal - cur.memAvail) / cur.memTotal * 100))
		total := uint64(cur.memTotal)
		free := uint64(cur.memAvail)
		row.MemoryTotalBytes = &total
		row.MemoryFreeBytes = &free
	}
	row.IOReadBytesPerSec = floatPtr(rate(cur.diskRead, prev.diskRead, dt))
	row.IOWriteBytesPerSec = floatPtr(rate(cur.diskWrite, prev.diskWrite, dt))
	row.IOUtilization = floatPtr(throughputToPercent(rate(cur.diskRead, prev.diskRead, dt)+rate(cur.diskWrite, prev.diskWrite, dt), c.diskCap))
	row.NetworkRxBytesPerSec = floatPtr(rate(cur.netRx, prev.netRx, dt))
	row.NetworkTxBytesPerSec = floatPtr(rate(cur.netTx, prev.netTx, dt))
	row.NetworkUtilization = floatPtr(throughputToPercent(rate(cur.netRx, prev.netRx, dt)+rate(cur.netTx, prev.netTx, dt), c.netCap))
}

func (c *Collector) computeContainerUsage(row *UtilizationRow, prev, cur rawContainerSample, dt float64) {
	if dt <= 0 || cur.cpuCount <= 0 || cur.memTotal <= 0 {
		return
	}
	row.CPUUtilization = floatPtr(clampPercent(rate(cur.cpuSeconds, prev.cpuSeconds, dt) / cur.cpuCount * 100))
	row.MemoryUtilization = floatPtr(clampPercent((cur.memTotal - cur.memAvailable) / cur.memTotal * 100))
	row.MemoryTotalBytes = uintPtr(uint64(cur.memTotal))
	row.MemoryFreeBytes = uintPtr(uint64(cur.memAvailable))
	rxRate := rate(cur.netRxBytes, prev.netRxBytes, dt)
	txRate := rate(cur.netTxBytes, prev.netTxBytes, dt)
	row.NetworkRxBytesPerSec = floatPtr(rxRate)
	row.NetworkTxBytesPerSec = floatPtr(txRate)
	row.NetworkUtilization = floatPtr(throughputToPercent(rxRate+txRate, c.netCap))
	readRate := rate(cur.diskReadBytes, prev.diskReadBytes, dt)
	writeRate := rate(cur.diskWriteBytes, prev.diskWriteBytes, dt)
	row.IOReadBytesPerSec = floatPtr(readRate)
	row.IOWriteBytesPerSec = floatPtr(writeRate)
	row.IOUtilization = floatPtr(throughputToPercent(readRate+writeRate, c.diskCap))
}

// Snapshot returns the current row set, sorted by host then container names.
func (c *Collector) Snapshot() MonitoringSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rows := make([]UtilizationRow, 0, len(c.rows))
	ready := false
	if host, ok := c.rows[defaultHostName]; ok {
		rows = append(rows, host)
		ready = rowReady(host)
	}
	names := make([]string, 0, len(c.rows))
	for name := range c.rows {
		if name != defaultHostName {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		row := c.rows[name]
		rows = append(rows, row)
		ready = ready || rowReady(row)
	}
	return MonitoringSnapshot{Ready: ready, Rows: rows}
}

func rowReady(row UtilizationRow) bool {
	return row.CPUUtilization != nil ||
		row.MemoryUtilization != nil ||
		row.IOUtilization != nil ||
		row.NetworkUtilization != nil
}

// parseMetrics parses the subset of LXD metrics needed by the monitoring table.
func parseMetrics(data []byte) map[string]rawContainerSample {
	out := make(map[string]rawContainerSample)
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		parsed := metricLineRE.FindStringSubmatch(line)
		if parsed == nil {
			continue
		}
		name := parsed[1]
		labelText := parsed[2]
		valText := parsed[3]

		labels := parseLabels(labelText)
		if labels["type"] != "container" {
			continue
		}
		containerName := labels["name"]
		if containerName == "" {
			continue
		}

		v, err := strconv.ParseFloat(valText, 64)
		if err != nil {
			continue
		}
		sample := out[containerName]
		switch name {
		case "lxd_cpu_seconds_total":
			sample.cpuSeconds += v
		case "lxd_cpu_effective_total":
			sample.cpuCount = v
		case "lxd_memory_MemTotal_bytes":
			sample.memTotal = v
		case "lxd_memory_MemAvailable_bytes":
			sample.memAvailable = v
		case "lxd_network_receive_bytes_total":
			if !isLoopbackDevice(labels["device"]) {
				sample.netRxBytes += v
			}
		case "lxd_network_transmit_bytes_total":
			if !isLoopbackDevice(labels["device"]) {
				sample.netTxBytes += v
			}
		case "lxd_disk_read_bytes_total":
			sample.diskReadBytes += v
		case "lxd_disk_write_bytes_total", "lxd_disk_written_bytes_total":
			sample.diskWriteBytes += v
		}
		out[containerName] = sample
	}
	return out
}

func parseLabels(s string) map[string]string {
	labels := make(map[string]string, 4)
	for _, m := range labelRE.FindAllStringSubmatch(s, -1) {
		labels[m[1]] = m[2]
	}
	return labels
}

func parseProcStat(raw []byte) (total float64, idle float64, err error) {
	var line string
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	if sc.Scan() {
		line = sc.Text()
	} else {
		return 0, 0, fmt.Errorf("proc/stat: missing data")
	}
	fields := strings.Fields(line)
	if len(fields) < 6 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("proc/stat: malformed cpu line")
	}
	for i := 1; i < len(fields); i++ {
		v, e := strconv.ParseFloat(fields[i], 64)
		if e != nil {
			return 0, 0, fmt.Errorf("proc/stat: parse field: %w", e)
		}
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	return total, idle, nil
}

func parseProcMeminfo(raw []byte) (total float64, available float64, err error) {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	var (
		gotTotal     bool
		usedFallback bool
	)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		switch parts[0] {
		case "MemTotal:":
			total, err = parseKib(parts[1:])
			if err != nil {
				return 0, 0, err
			}
			gotTotal = true
		case "MemAvailable:":
			available, err = parseKib(parts[1:])
			if err != nil {
				return 0, 0, err
			}
			usedFallback = true
		case "MemFree:":
			// Fallback if MemAvailable is not exposed.
			if !usedFallback {
				available, err = parseKib(parts[1:])
				if err != nil {
					return 0, 0, err
				}
			}
		}
	}
	if !gotTotal || available <= 0 {
		return 0, 0, fmt.Errorf("proc/meminfo: incomplete data")
	}
	return total, available, nil
}

func parseKib(parts []string) (float64, error) {
	if len(parts) == 0 {
		return 0, fmt.Errorf("empty value")
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, err
	}
	return v * 1024, nil
}

func parseProcDiskstats(raw []byte) (readBytes float64, writeBytes float64, err error) {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}
		device := fields[2]
		if isIgnoredBlockDevice(device) {
			continue
		}
		readSectors, e := strconv.ParseFloat(fields[5], 64)
		if e != nil {
			return 0, 0, fmt.Errorf("proc/diskstats read sectors: %w", e)
		}
		writeSectors, e := strconv.ParseFloat(fields[9], 64)
		if e != nil {
			return 0, 0, fmt.Errorf("proc/diskstats write sectors: %w", e)
		}
		readBytes += readSectors * 512
		writeBytes += writeSectors * 512
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	return readBytes, writeBytes, nil
}

func parseProcNetDev(raw []byte) (rxBytes float64, txBytes float64, err error) {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	// header has 2 lines.
	for i := 0; i < 2 && sc.Scan(); i++ {
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		device := strings.TrimSpace(parts[0])
		if device == "lo" {
			continue
		}
		stats := strings.Fields(parts[1])
		if len(stats) < 16 {
			continue
		}
		rx, e := strconv.ParseFloat(stats[0], 64)
		if e != nil {
			return 0, 0, fmt.Errorf("proc/net/dev rx: %w", e)
		}
		tx, e := strconv.ParseFloat(stats[8], 64)
		if e != nil {
			return 0, 0, fmt.Errorf("proc/net/dev tx: %w", e)
		}
		rxBytes += rx
		txBytes += tx
	}
	return rxBytes, txBytes, nil
}

func rateToPercent(delta float64, idledelta float64, dt float64) *float64 {
	if dt <= 0 || delta <= 0 {
		return nil
	}
	return floatPtr(clampPercent((delta - idledelta) / delta * 100))
}

func rate(cur, prev, dt float64) float64 {
	if dt <= 0 || cur < prev {
		return 0
	}
	return (cur - prev) / dt
}

func throughputToPercent(v float64, capBytes float64) float64 {
	if capBytes <= 0 {
		return 0
	}
	return clampPercent(v / capBytes * 100)
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

// UtilizationClass maps utilization to alerting classes.
func UtilizationClass(v float64) string {
	switch {
	case v >= 80:
		return "critical"
	case v >= 70:
		return "warning"
	default:
		return "normal"
	}
}

func isZeroHostSample(s rawHostSample) bool {
	return s.cpuTotal == 0 && s.cpuIdle == 0 && s.memTotal == 0 && s.memAvail == 0 && s.diskRead == 0 && s.diskWrite == 0 && s.netRx == 0 && s.netTx == 0
}

func floatPtr(v float64) *float64 {
	return &v
}

func uintPtr(v uint64) *uint64 {
	return &v
}

func isLoopbackDevice(device string) bool {
	return device == "lo"
}

func isIgnoredBlockDevice(device string) bool {
	return strings.HasPrefix(device, "loop") || strings.HasPrefix(device, "ram")
}

var metricLineRE = regexp.MustCompile(`^(lxd_[a-zA-Z_]+)\{([^}]*)\}\s+([0-9eE.+-]+)\s*$`)
var labelRE = regexp.MustCompile(`(\w+)="([^"]*)"`)

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
