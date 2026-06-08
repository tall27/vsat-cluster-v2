package metrics

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	outputs [][]byte
	calls   int
}

func (f *fakeRunner) Run(_ context.Context, _ ...string) ([]byte, error) {
	if len(f.outputs) == 0 {
		return []byte{}, nil
	}
	if f.calls >= len(f.outputs) {
		return f.outputs[len(f.outputs)-1], nil
	}
	out := f.outputs[f.calls]
	f.calls++
	return out, nil
}

const lxdMetricsFixture = `
# HELP lxd_cpu_effective_total CPUs available
lxd_cpu_effective_total{name="vsat-a",project="default",type="container"} 2
lxd_cpu_effective_total{name="vsat-b",project="default",type="container"} 1
lxd_cpu_seconds_total{cpu="0",name="vsat-a",project="default",type="container"} 100
lxd_cpu_seconds_total{cpu="1",name="vsat-a",project="default",type="container"} 300
lxd_cpu_seconds_total{cpu="0",name="vsat-b",project="default",type="container"} 400
lxd_memory_MemTotal_bytes{name="vsat-a",project="default",type="container"} 1024
lxd_memory_MemAvailable_bytes{name="vsat-a",project="default",type="container"} 256
lxd_memory_MemTotal_bytes{name="vsat-b",project="default",type="container"} 2048
lxd_memory_MemAvailable_bytes{name="vsat-b",project="default",type="container"} 1024
lxd_network_receive_bytes_total{device="eth0",name="vsat-a",project="default",type="container"} 1000
lxd_network_receive_bytes_total{device="lo",name="vsat-a",project="default",type="container"} 9999
lxd_network_transmit_bytes_total{device="eth0",name="vsat-a",project="default",type="container"} 1500
lxd_disk_read_bytes_total{name="vsat-a",project="default",type="container"} 4096
lxd_disk_written_bytes_total{name="vsat-a",project="default",type="container"} 2048
lxd_network_receive_bytes_total{device="eth0",name="vsat-b",project="default",type="container"} 50
lxd_network_transmit_bytes_total{device="eth0",name="vsat-b",project="default",type="container"} 60
lxd_disk_read_bytes_total{name="vsat-b",project="default",type="container"} 3000
lxd_disk_written_bytes_total{name="vsat-b",project="default",type="container"} 3000
lxd_cpu_seconds_total{cpu="0",name="ignored",project="default",type="virtual-machine"} 9001
`

func TestParseMetricsParsesContainerCPUAndCounters(t *testing.T) {
	parsed := parseMetrics([]byte(lxdMetricsFixture))
	a, ok := parsed["vsat-a"]
	if !ok {
		t.Fatalf("expected vsat-a sample, got %v", parsed)
	}
	if a.cpuSeconds != 400 {
		t.Fatalf("cpu seconds=%v want=400", a.cpuSeconds)
	}
	if a.cpuCount != 2 {
		t.Fatalf("cpu count=%v want=2", a.cpuCount)
	}
	if a.memTotal != 1024 {
		t.Fatalf("memTotal=%v want=1024", a.memTotal)
	}
	if a.memAvailable != 256 {
		t.Fatalf("memAvailable=%v want=256", a.memAvailable)
	}
	if a.netRxBytes != 1000 {
		t.Fatalf("rx=%v want=1000", a.netRxBytes)
	}
	if a.netTxBytes != 1500 {
		t.Fatalf("tx=%v want=1500", a.netTxBytes)
	}
	if a.diskReadBytes != 4096 {
		t.Fatalf("disk read=%v want=4096", a.diskReadBytes)
	}
	if a.diskWriteBytes != 2048 {
		t.Fatalf("disk write=%v want=2048", a.diskWriteBytes)
	}
	if _, ok := parsed["ignored"]; ok {
		t.Fatalf("non-container data should be filtered out")
	}
}

func TestParseProcStatParsesCPU(t *testing.T) {
	raw := []byte("cpu  1024 200 300 400 500 600 700 800 900 1000 1100\n")
	total, idle, err := parseProcStat(raw)
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if total != 7524 {
		t.Fatalf("total=%v want=%d", total, 7524)
	}
	if idle != 900 {
		t.Fatalf("idle=%v want=%d", idle, 900)
	}
}

func TestParseProcMeminfoParsesTotals(t *testing.T) {
	raw := []byte(`
MemTotal:       2048000 kB
MemFree:        800000 kB
MemAvailable:   1024000 kB
`)
	total, free, err := parseProcMeminfo(raw)
	if err != nil {
		t.Fatalf("parseProcMeminfo: %v", err)
	}
	if total != 2.097152e+09 {
		t.Fatalf("total=%v want=%g", total, 2.097152e+09)
	}
	if free != 1.048576e+09 {
		t.Fatalf("free=%v want=%g", free, 1.048576e+09)
	}
}

func TestParseProcMeminfoFallsBackToMemFree(t *testing.T) {
	raw := []byte(`
MemTotal:       2048000 kB
MemFree:        1500000 kB
`)
	total, free, err := parseProcMeminfo(raw)
	if err != nil {
		t.Fatalf("parseProcMeminfo: %v", err)
	}
	if total != 2.097152e+09 {
		t.Fatalf("total=%v want=%g", total, 2.097152e+09)
	}
	if free != 1.536e+09 {
		t.Fatalf("free=%v want=%g", free, 1.536e+09)
	}
}

func TestParseProcDiskstatsParsesBytes(t *testing.T) {
	raw := []byte(`
8 0 sda 100 0 200 0 0 0 300 0 0 0 0 0 0
8 1 sdb1 2 0 10 0 0 0 20 0 0 0 0 0 0
7 0 loop0 1 0 10 0 0 0 20 0 0 0 0 0 0
`)
	read, write, err := parseProcDiskstats(raw)
	if err != nil {
		t.Fatalf("parseProcDiskstats: %v", err)
	}
	if read != 107520 {
		t.Fatalf("read=%v want=%v", read, 107520)
	}
	if write != 163840 {
		t.Fatalf("write=%v want=%v", write, 163840)
	}
}

func TestParseProcNetDevParsesBytes(t *testing.T) {
	raw := []byte(`
Inter-|   Receive                                                | Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
   lo:    100      0    0    0    0    0     0          0         10      0    0    0    0    0    0       0
 eth0:    200      0    0    0    0    0     0          0         400     0    0    0    0    0    0       0
`)
	rx, tx, err := parseProcNetDev(raw)
	if err != nil {
		t.Fatalf("parseProcNetDev: %v", err)
	}
	if rx != 200 {
		t.Fatalf("rx=%v want=200", rx)
	}
	if tx != 400 {
		t.Fatalf("tx=%v want=400", tx)
	}
}

func TestUtilizationClassThresholds(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{69.9, "normal"},
		{70, "warning"},
		{79.9, "warning"},
		{80, "critical"},
	} {
		t.Run(strconv.FormatFloat(tc.v, 'f', 1, 64), func(t *testing.T) {
			if got := UtilizationClass(tc.v); got != tc.want {
				t.Fatalf("UtilizationClass(%v) = %s, want %s", tc.v, got, tc.want)
			}
		})
	}
}

func TestCollectorSnapshotIncludesHostAndContainers(t *testing.T) {
	c := NewCollector(Options{
		Runner: &fakeRunner{outputs: [][]byte{[]byte(lxdMetricsFixture), []byte(lxdMetricsFixture)}},
		ReadFile: func(name string) ([]byte, error) {
			switch {
			case strings.Contains(name, "/proc/stat"):
				return []byte("cpu  1 2 3 4 5 6 7 8 9 10 11\n"), nil
			case strings.Contains(name, "/proc/meminfo"):
				return []byte("MemTotal:       1024 kB\nMemAvailable:  512 kB\n"), nil
			case strings.Contains(name, "/proc/diskstats"):
				return []byte("8 0 sda 1 0 2 0 0 0 3 0 0 0 0 0 0\n"), nil
			case strings.Contains(name, "/proc/net/dev"):
				return []byte(`
Inter-|   Receive                                                | Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
 eth0:    10      0    0    0    0    0     0          0         20     0    0    0    0    0    0       0
`), nil
			default:
				return nil, nil
			}
		},
		ListFn: func(_ context.Context) ([]ContainerInfo, error) {
			return []ContainerInfo{
				{Name: "vsat-a", Status: "Running"},
				{Name: "vsat-b", Status: "Stopped"},
			}, nil
		},
		Interval: 60 * time.Second,
	})
	c.poll(context.Background())
	snapshot := c.Snapshot()
	if len(snapshot.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(snapshot.Rows))
	}
	if snapshot.Rows[0].Type != "Host" || snapshot.Rows[0].Name != "host" {
		t.Fatalf("first row should be host, got %+v", snapshot.Rows[0])
	}
	if snapshot.Rows[1].Name != "vsat-a" || snapshot.Rows[2].Name != "vsat-b" {
		t.Fatalf("expected vsat-a and vsat-b rows, got %+v", snapshot.Rows)
	}
	if snapshot.Rows[2].Status != "Stopped" {
		t.Fatalf("stopped row should preserve status")
	}
}
