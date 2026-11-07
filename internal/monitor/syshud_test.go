package monitor

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseStatAndPercent(t *testing.T) {
	a, err := ParseStat([]byte("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 1 1 1\n"))
	if err != nil || a.Total != 1000 || a.Idle != 800 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	b, _ := ParseStat([]byte("cpu  200 0 200 800 200 0 0 0 0 0\n"))
	// delta total 400, delta idle 200 -> 50%
	if p := CPUPercent(a, b); p != 50 {
		t.Fatalf("pct=%v", p)
	}
	if CPUPercent(b, a) != 0 {
		t.Fatal("counter going backwards must yield 0")
	}
}

func TestParseMemInfo(t *testing.T) {
	m, err := ParseMemInfo([]byte("MemTotal:  1000 kB\nMemFree: 10 kB\nMemAvailable:  250 kB\nBuffers: 5 kB\nCached: 7 kB\n"))
	if err != nil || m.UsedKB() != 750 || m.BuffersKB != 5 || m.CachedKB != 7 || m.Percent() != 75 {
		t.Fatalf("%+v %v", m, err)
	}
}

const netdev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  500  5 0 0 0 0 0 0  500  5 0 0 0 0 0 0
  eth0: 2048 10 0 0 0 0 0 0 4096 12 0 0 0 0 0 0
`

func TestParseNetDev(t *testing.T) {
	c, ok := ParseNetDev([]byte(netdev), "eth0")
	if !ok || c.RX != 2048 || c.TX != 4096 {
		t.Fatalf("%+v %v", c, ok)
	}
	c, ok = ParseNetDev([]byte(netdev), "wlan9") // falls back to first non-lo
	if !ok || c.Name != "eth0" {
		t.Fatalf("%+v %v", c, ok)
	}
}

func TestParseUptime(t *testing.T) {
	d, err := ParseUptime([]byte("123.45 200.00\n"))
	if err != nil || d.Seconds() != 123.45 {
		t.Fatal(d, err)
	}
}

func TestMonitorRun(t *testing.T) {
	var buf bytes.Buffer
	calls := 0
	wait := func(d time.Duration) bool {
		calls++
		return calls >= 2
	}
	err := Run(&buf, "eth0", time.Millisecond, wait)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "myos syshud") || !strings.Contains(out, "CPU") || !strings.Contains(out, "RAM") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestReadCPUTimesAndMemInfo(t *testing.T) {
	if _, err := ReadCPUTimes(); err != nil {
		t.Logf("ReadCPUTimes on host: %v", err)
	}
	if _, err := ReadMemInfo(); err != nil {
		t.Logf("ReadMemInfo on host: %v", err)
	}
}
