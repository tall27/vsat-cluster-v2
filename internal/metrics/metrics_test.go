package metrics

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const samplePrometheusText = `
# HELP lxd_cpu_effective_total CPUs available
# TYPE lxd_cpu_effective_total gauge
lxd_cpu_effective_total{name="vsat-a",project="default",type="container"} 2
# HELP lxd_cpu_seconds_total CPU usage
# TYPE lxd_cpu_seconds_total counter
lxd_cpu_seconds_total{cpu="0",name="vsat-a",project="default",type="container"} %f
lxd_cpu_seconds_total{cpu="1",name="vsat-a",project="default",type="container"} 0
# HELP lxd_memory_MemTotal_bytes Memory total
lxd_memory_MemTotal_bytes{name="vsat-a",project="default",type="container"} 1000
lxd_memory_MemAvailable_bytes{name="vsat-a",project="default",type="container"} %f
# HELP lxd_network_receive_bytes_total Bytes received
lxd_network_receive_bytes_total{device="eth0",name="vsat-a",project="default",type="container"} %f
lxd_network_receive_bytes_total{device="lo",name="vsat-a",project="default",type="container"} 999999
lxd_network_transmit_bytes_total{device="eth0",name="vsat-a",project="default",type="container"} %f
lxd_network_receive_packets_total{device="eth0",name="vsat-a",project="default",type="container"} %f
lxd_network_transmit_packets_total{device="eth0",name="vsat-a",project="default",type="container"} %f
lxd_cpu_seconds_total{cpu="0",name="other",project="default",type="virtual-machine"} 5000
`

// fakeRunner returns canned Prometheus text on each call, in order.
type fakeRunner struct {
	mu      sync.Mutex
	outputs [][]byte
	calls   int
}

func (f *fakeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls >= len(f.outputs) {
		return f.outputs[len(f.outputs)-1], nil
	}
	out := f.outputs[f.calls]
	f.calls++
	return out, nil
}

func render(cpuSeconds, memAvail, rxBytes, txBytes, rxPackets, txPackets float64) []byte {
	s := samplePrometheusText
	for _, v := range []float64{cpuSeconds, memAvail, rxBytes, txBytes, rxPackets, txPackets} {
		s = strings.Replace(s, "%f", strconv.FormatFloat(v, 'f', -1, 64), 1)
	}
	return []byte(s)
}

func TestParseMetricsFiltersToContainerAndEth0(t *testing.T) {
	out := parseMetrics(render(10, 700, 1000, 2000, 10, 20))
	s, ok := out["vsat-a"]
	if !ok {
		t.Fatalf("expected sample for vsat-a, got %v", out)
	}
	if s.cpuSeconds != 10 {
		t.Errorf("cpuSeconds = %v, want 10 (cpu=1 line is 0)", s.cpuSeconds)
	}
	if s.cpuCount != 2 {
		t.Errorf("cpuCount = %v, want 2", s.cpuCount)
	}
	if s.memAvailable != 700 {
		t.Errorf("memAvailable = %v, want 700", s.memAvailable)
	}
	if s.netRxBytes != 1000 {
		t.Errorf("netRxBytes = %v, want 1000 (lo device must be excluded)", s.netRxBytes)
	}
	if _, ok := out["other"]; ok {
		t.Errorf("virtual-machine type should be filtered out")
	}
}

func TestCollectorDerivesRatesAcrossPolls(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		render(10, 800, 1000, 2000, 10, 20),
		render(12, 600, 1200, 2400, 30, 60),
	}}
	c := NewCollector(Options{Runner: runner, Window: 10})

	ctx := context.Background()
	c.poll(ctx)
	if _, ok := c.Snapshot("vsat-a"); ok {
		t.Fatalf("expected no snapshot after a single poll (need a delta)")
	}

	// Force a deterministic dt by directly invoking record with a synthetic time.
	c.mu.Lock()
	c.prev["vsat-a"] = rawSample{
		at:         time.Now().Add(-10 * time.Second),
		cpuSeconds: 10, cpuCount: 2,
		memTotal: 1000, memAvailable: 800,
		netRxBytes: 1000, netTxBytes: 2000,
		netRxPackets: 10, netTxPackets: 20,
	}
	c.mu.Unlock()

	c.record("vsat-a", rawSample{
		at:         time.Now(),
		cpuSeconds: 12, cpuCount: 2,
		memTotal: 1000, memAvailable: 600,
		netRxBytes: 1200, netTxBytes: 2400,
		netRxPackets: 30, netTxPackets: 60,
	})

	snap, ok := c.Snapshot("vsat-a")
	if !ok {
		t.Fatalf("expected a snapshot after two samples")
	}
	if len(snap.CPUPercent) != 1 {
		t.Fatalf("expected 1 cpu point, got %d", len(snap.CPUPercent))
	}
	// (12-10)/10s = 0.2 cores / 2 cores * 100 = 10%
	if got := snap.CPUPercent[0].Value; got < 9.9 || got > 10.1 {
		t.Errorf("cpuPercent = %v, want ~10", got)
	}
	// used = 1000-600 = 400 / 1000 = 40%
	if got := snap.MemoryPercent[0].Value; got < 39.9 || got > 40.1 {
		t.Errorf("memoryPercent = %v, want ~40", got)
	}
	// (1200-1000)/10s = 20 bytes/sec
	if got := snap.NetRxBytesPerSec[0].Value; got < 19.9 || got > 20.1 {
		t.Errorf("netRxBytesPerSec = %v, want ~20", got)
	}
}

func TestRateClampsCounterResetToZero(t *testing.T) {
	if got := rate(5, 100, 10); got != 0 {
		t.Errorf("rate() on counter reset = %v, want 0", got)
	}
	if got := rate(150, 100, 10); got != 5 {
		t.Errorf("rate() = %v, want 5", got)
	}
}

func TestSeriesRingBufferCapsAtWindow(t *testing.T) {
	s := newSeries(3)
	for i := 0; i < 5; i++ {
		s.add(time.Now(), float64(i))
	}
	pts := s.snapshot()
	if len(pts) != 3 {
		t.Fatalf("len = %d, want 3", len(pts))
	}
	if pts[0].Value != 2 || pts[2].Value != 4 {
		t.Errorf("ring buffer kept wrong window: %+v", pts)
	}
}

func TestSnapshotUnknownContainer(t *testing.T) {
	c := NewCollector(Options{Runner: &fakeRunner{outputs: [][]byte{[]byte("")}}})
	if _, ok := c.Snapshot("nope"); ok {
		t.Errorf("expected no snapshot for unknown container")
	}
}
