// Package monitor implements syshud, a flicker-free ANSI telemetry dashboard
// fed directly by /proc (no external tools).
package monitor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func parseUintBytes(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, strconv.ErrSyntax
	}
	var v uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, strconv.ErrSyntax
		}
		v = v*10 + uint64(c-'0')
	}
	return v, nil
}

func nextField(b []byte) (field, rest []byte) {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	b = b[i:]
	j := 0
	for j < len(b) && b[j] != ' ' && b[j] != '\t' && b[j] != '\r' {
		j++
	}
	return b[:j], b[j:]
}

// CPUTimes is the aggregate jiffy counters from the first line of /proc/stat.
type CPUTimes struct{ Idle, Total uint64 }

// ParseStat parses the aggregate "cpu " line of /proc/stat without heap allocations.
func ParseStat(data []byte) (CPUTimes, error) {
	for len(data) > 0 {
		var line []byte
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line, data = data[:idx], data[idx+1:]
		} else {
			line, data = data, nil
		}
		if !bytes.HasPrefix(line, []byte("cpu ")) {
			continue
		}
		rest := line[4:]
		var t CPUTimes
		count := 0
		for count < 8 {
			tok, r := nextField(rest)
			if len(tok) == 0 {
				break
			}
			rest = r
			v, err := parseUintBytes(tok)
			if err != nil {
				return CPUTimes{}, err
			}
			t.Total += v
			if count == 3 || count == 4 { // idle + iowait
				t.Idle += v
			}
			count++
		}
		if count < 4 {
			break
		}
		return t, nil
	}
	return CPUTimes{}, fmt.Errorf("no cpu line in /proc/stat")
}

// CPUPercent returns utilisation (0..100) between two samples.
func CPUPercent(prev, cur CPUTimes) float64 {
	dt := cur.Total - prev.Total
	if cur.Total < prev.Total || dt == 0 {
		return 0
	}
	di := cur.Idle - prev.Idle
	if cur.Idle < prev.Idle || di > dt {
		return 0
	}
	return float64(dt-di) * 100 / float64(dt)
}

// ReadCPUTimes reads /proc/stat and returns the aggregate CPU counters.
func ReadCPUTimes() (CPUTimes, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return CPUTimes{}, err
	}
	return ParseStat(b)
}

// MemInfo holds the /proc/meminfo fields we display (kB).
type MemInfo struct{ TotalKB, AvailKB, BuffersKB, CachedKB uint64 }

// UsedKB is MemTotal - MemAvailable.
func (m MemInfo) UsedKB() uint64 {
	if m.AvailKB > m.TotalKB {
		return 0
	}
	return m.TotalKB - m.AvailKB
}

// Percent is used memory as a percentage of total.
func (m MemInfo) Percent() float64 {
	if m.TotalKB == 0 {
		return 0
	}
	return float64(m.UsedKB()) * 100 / float64(m.TotalKB)
}

// ParseMemInfo parses /proc/meminfo without per-line string allocations.
func ParseMemInfo(data []byte) (MemInfo, error) {
	var m MemInfo
	for len(data) > 0 {
		var line []byte
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line, data = data[:idx], data[idx+1:]
		} else {
			line, data = data, nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		k, rest := line[:colon], line[colon+1:]
		var dst *uint64
		switch {
		case bytes.Equal(k, []byte("MemTotal")):
			dst = &m.TotalKB
		case bytes.Equal(k, []byte("MemAvailable")):
			dst = &m.AvailKB
		case bytes.Equal(k, []byte("Buffers")):
			dst = &m.BuffersKB
		case bytes.Equal(k, []byte("Cached")):
			dst = &m.CachedKB
		default:
			continue
		}
		tok, _ := nextField(rest)
		if len(tok) == 0 {
			continue
		}
		v, err := parseUintBytes(tok)
		if err != nil {
			return m, err
		}
		*dst = v
	}
	if m.TotalKB == 0 {
		return m, fmt.Errorf("MemTotal missing in /proc/meminfo")
	}
	return m, nil
}

// ReadMemInfo reads /proc/meminfo.
func ReadMemInfo() (MemInfo, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemInfo{}, err
	}
	return ParseMemInfo(b)
}

// NetCounters are cumulative byte counters for one interface.
type NetCounters struct {
	Name   string
	RX, TX uint64
}

// ParseNetDev finds iface in /proc/net/dev. If iface is absent (or empty) the
// first non-loopback interface is returned instead.
func ParseNetDev(data []byte, iface string) (NetCounters, bool) {
	var fallback NetCounters
	haveFallback := false
	for len(data) > 0 {
		var line []byte
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line, data = data[:idx], data[idx+1:]
		} else {
			line, data = data, nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue // header lines
		}
		nameBytes := bytes.TrimSpace(line[:colon])
		rest := line[colon+1:]
		var rx, tx uint64
		okFields := true
		for i := 0; i <= 8; i++ {
			var tok []byte
			tok, rest = nextField(rest)
			if len(tok) == 0 {
				okFields = false
				break
			}
			if i == 0 {
				v, err := parseUintBytes(tok)
				if err != nil {
					okFields = false
					break
				}
				rx = v
			} else if i == 8 {
				v, err := parseUintBytes(tok)
				if err != nil {
					okFields = false
					break
				}
				tx = v
			}
		}
		if !okFields {
			continue
		}
		name := string(nameBytes)
		c := NetCounters{Name: name, RX: rx, TX: tx}
		if name == iface {
			return c, true
		}
		if name != "lo" && !haveFallback {
			fallback, haveFallback = c, true
		}
	}
	return fallback, haveFallback
}

// ParseUptime parses /proc/uptime.
func ParseUptime(data []byte) (time.Duration, error) {
	f := strings.Fields(string(data))
	if len(f) < 1 {
		return 0, fmt.Errorf("empty /proc/uptime")
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(s * float64(time.Second)), nil
}

// CountProcs returns the active process count using sysinfo(2) when querying /proc,
// or counts numeric directories under custom procDir paths in tests.
func CountProcs(procDir string) int {
	if procDir == "" || procDir == "/proc" {
		var si syscall.Sysinfo_t
		if err := syscall.Sysinfo(&si); err == nil && si.Procs > 0 {
			return int(si.Procs)
		}
	}
	d, err := os.Open(procDir)
	if err != nil {
		return 0
	}
	defer d.Close()
	names, _ := d.Readdirnames(-1)
	n := 0
	for _, nm := range names {
		if _, err := strconv.Atoi(nm); err == nil {
			n++
		}
	}
	return n
}

// Waiter blocks for up to d and reports whether the user asked to quit.
type Waiter func(d time.Duration) (quit bool)

const (
	ansiHome   = "\x1b[H"
	ansiEOL    = "\x1b[K\r\n"
	ansiReset  = "\x1b[0m"
	barWidth   = 40
	altScreen  = "\x1b[?1049h\x1b[2J\x1b[?25l"
	leaveAlt   = "\x1b[?25h\x1b[?1049l"
	colHeading = "\x1b[1;36m"
)

func bar(pct float64, buf *bytes.Buffer) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct*barWidth/100 + 0.5)
	color := "\x1b[32m"
	if pct >= 85 {
		color = "\x1b[31m"
	} else if pct >= 60 {
		color = "\x1b[33m"
	}
	buf.WriteString("[" + color)
	buf.WriteString(strings.Repeat("|", filled))
	buf.WriteString("\x1b[90m")
	buf.WriteString(strings.Repeat(".", barWidth-filled))
	buf.WriteString(ansiReset + "]")
}

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%dd %02dh %02dm %02ds", s/86400, s/3600%24, s/60%60, s%60)
}

func mb(kb uint64) float64 { return float64(kb) / 1024 }

// Run draws the dashboard every interval until wait reports quit. The
// alternate screen and cursor are always restored on return. Each frame is
// assembled in memory and written with a single write(2), overwriting in
// place, so there is no flicker.
func Run(out io.Writer, iface string, interval time.Duration, wait Waiter) error {
	if iface == "" {
		iface = "eth0"
	}
	if _, err := io.WriteString(out, altScreen); err != nil {
		return err
	}
	defer io.WriteString(out, leaveAlt+ansiReset)

	var (
		buf      bytes.Buffer
		prevCPU  CPUTimes
		prevNet  NetCounters
		prevTime time.Time
		haveNet  bool
		cpuPct   float64
		rxKBs    float64
		txKBs    float64
	)
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		prevCPU, _ = ParseStat(b)
	}
	prevTime = time.Now()

	for first := true; ; first = false {
		if first {
			// Short first tick so the screen fills quickly.
			if wait(250 * time.Millisecond) {
				return nil
			}
		}
		now := time.Now()
		dt := now.Sub(prevTime).Seconds()
		if dt <= 0 {
			dt = 1
		}

		if b, err := os.ReadFile("/proc/stat"); err == nil {
			if cur, err := ParseStat(b); err == nil {
				cpuPct = CPUPercent(prevCPU, cur)
				prevCPU = cur
			}
		}
		mem, memErr := ReadMemInfo()
		var nc NetCounters
		haveCur := false
		if b, err := os.ReadFile("/proc/net/dev"); err == nil {
			nc, haveCur = ParseNetDev(b, iface)
		}
		if haveCur {
			if haveNet && prevNet.Name == nc.Name && nc.RX >= prevNet.RX && nc.TX >= prevNet.TX {
				rxKBs = float64(nc.RX-prevNet.RX) / 1024 / dt
				txKBs = float64(nc.TX-prevNet.TX) / 1024 / dt
			} else {
				rxKBs, txKBs = 0, 0
			}
			prevNet, haveNet = nc, true
		} else {
			haveNet = false
		}
		prevTime = now

		var up time.Duration
		if b, err := os.ReadFile("/proc/uptime"); err == nil {
			up, _ = ParseUptime(b)
		}

		buf.Reset()
		buf.WriteString(ansiHome)
		fmt.Fprintf(&buf, "%s myos syshud %s  %s%s", colHeading, ansiReset, now.Format("15:04:05"), ansiEOL)
		buf.WriteString(ansiEOL)
		fmt.Fprintf(&buf, " CPU  ")
		bar(cpuPct, &buf)
		fmt.Fprintf(&buf, " %5.1f%%%s", cpuPct, ansiEOL)
		buf.WriteString(" RAM  ")
		if memErr == nil {
			bar(mem.Percent(), &buf)
			fmt.Fprintf(&buf, " %5.1f%%%s", mem.Percent(), ansiEOL)
			fmt.Fprintf(&buf, "      used %.0f / %.0f MB   buffers %.0f MB   cached %.0f MB%s",
				mb(mem.UsedKB()), mb(mem.TotalKB), mb(mem.BuffersKB), mb(mem.CachedKB), ansiEOL)
		} else {
			fmt.Fprintf(&buf, "unavailable: %v%s", memErr, ansiEOL)
		}
		buf.WriteString(ansiEOL)
		if haveCur {
			fmt.Fprintf(&buf, " NET  %-8s \x1b[32mRX %9.1f KB/s\x1b[0m   \x1b[35mTX %9.1f KB/s\x1b[0m%s", nc.Name, rxKBs, txKBs, ansiEOL)
		} else {
			fmt.Fprintf(&buf, " NET  %-8s (not present)%s", iface, ansiEOL)
		}
		buf.WriteString(ansiEOL)
		fmt.Fprintf(&buf, " PROCS %-6d UPTIME %s%s", CountProcs("/proc"), fmtDur(up), ansiEOL)
		buf.WriteString(ansiEOL)
		buf.WriteString("\x1b[90m press q or Ctrl+C to quit" + ansiReset + ansiEOL)
		buf.WriteString("\x1b[J")
		if _, err := out.Write(buf.Bytes()); err != nil {
			return err
		}
		if wait(interval) {
			return nil
		}
	}
}
