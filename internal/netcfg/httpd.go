package netcfg

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"myos/internal/monitor"
	"myos/internal/sys"
)

// NetIfaceStats holds RX/TX counters for one network interface from /proc/net/dev.
type NetIfaceStats struct {
	Name      string `json:"name"`
	RXBytes   uint64 `json:"rx_bytes"`
	RXPackets uint64 `json:"rx_packets"`
	TXBytes   uint64 `json:"tx_bytes"`
	TXPackets uint64 `json:"tx_packets"`
}

// MemMetrics holds memory breakdown in KiB.
type MemMetrics struct {
	TotalKB     uint64 `json:"total_kb"`
	UsedKB      uint64 `json:"used_kb"`
	AvailableKB uint64 `json:"available_kb"`
	BuffersKB   uint64 `json:"buffers_kb"`
	CachedKB    uint64 `json:"cached_kb"`
}

// MetricsPayload is the JSON response returned by GET /api/metrics.
type MetricsPayload struct {
	Timestamp  int64             `json:"timestamp"`
	UptimeSec  int64             `json:"uptime_sec"`
	Procs      uint16            `json:"procs"`
	CPUPercent float64           `json:"cpu_percent"`
	Memory     MemMetrics        `json:"memory"`
	Net        []NetIfaceStats   `json:"net"`
	Disks      []sys.BlockDevice `json:"disks"`
}

// TelemetryServer manages the embedded HTTP telemetry control plane.
type TelemetryServer struct {
	mu      sync.Mutex
	srv     *http.Server
	addr    string
	running bool
	done    chan struct{}
	prevCPU monitor.CPUTimes
}

// DefaultHTTPD is the global telemetry HTTP server instance.
var DefaultHTTPD = NewTelemetryServer()

// NewTelemetryServer creates a new TelemetryServer.
func NewTelemetryServer() *TelemetryServer {
	return &TelemetryServer{}
}

// Start starts the HTTP server on addr (default ":8080").
func (ts *TelemetryServer) Start(addr string) error {
	if addr == "" {
		addr = ":8080"
	} else if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}

	ts.mu.Lock()
	if ts.running {
		cur := ts.addr
		ts.mu.Unlock()
		return fmt.Errorf("httpd already running on %s", cur)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		ts.mu.Unlock()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", ts.handleDashboard)
	mux.HandleFunc("/api/metrics", ts.handleMetrics)

	srv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	done := make(chan struct{})
	ts.srv = srv
	ts.addr = ln.Addr().String()
	ts.running = true
	ts.done = done
	if cpu, err := monitor.ReadCPUTimes(); err == nil {
		ts.prevCPU = cpu
	}
	ts.mu.Unlock()

	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	return nil
}

// Stop gracefully shuts down the HTTP server using ctx.
func (ts *TelemetryServer) Stop(ctx context.Context) error {
	ts.mu.Lock()
	if !ts.running || ts.srv == nil {
		ts.mu.Unlock()
		return fmt.Errorf("httpd is not running")
	}
	srv := ts.srv
	done := ts.done
	ts.running = false
	ts.srv = nil
	ts.mu.Unlock()

	err := srv.Shutdown(ctx)
	if done != nil {
		<-done
	}
	return err
}

// Status reports whether the server is running and its bound address.
func (ts *TelemetryServer) Status() (running bool, addr string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.running, ts.addr
}

func (ts *TelemetryServer) collectMetrics() MetricsPayload {
	p := MetricsPayload{
		Timestamp: time.Now().Unix(),
	}

	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err == nil {
		p.UptimeSec = int64(si.Uptime)
		p.Procs = si.Procs
	}

	if curCPU, err := monitor.ReadCPUTimes(); err == nil {
		ts.mu.Lock()
		if ts.prevCPU.Total > 0 {
			p.CPUPercent = monitor.CPUPercent(ts.prevCPU, curCPU)
		}
		ts.prevCPU = curCPU
		ts.mu.Unlock()
	}

	if mem, err := monitor.ReadMemInfo(); err == nil {
		p.Memory = MemMetrics{
			TotalKB:     mem.TotalKB,
			UsedKB:      mem.UsedKB(),
			AvailableKB: mem.AvailKB,
			BuffersKB:   mem.BuffersKB,
			CachedKB:    mem.CachedKB,
		}
	}

	p.Net = readNetDev("/proc/net/dev")
	if disks, err := sys.ScanDisks(); err == nil {
		p.Disks = disks
	}
	if p.Net == nil {
		p.Net = []NetIfaceStats{}
	}
	if p.Disks == nil {
		p.Disks = []sys.BlockDevice{}
	}
	return p
}

func readNetDev(path string) []NetIfaceStats {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []NetIfaceStats
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo <= 2 {
			continue // skip header lines
		}
		line := sc.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 10 {
			continue
		}
		rxB, _ := strconv.ParseUint(fields[0], 10, 64)
		rxP, _ := strconv.ParseUint(fields[1], 10, 64)
		txB, _ := strconv.ParseUint(fields[8], 10, 64)
		txP, _ := strconv.ParseUint(fields[9], 10, 64)
		out = append(out, NetIfaceStats{
			Name:      name,
			RXBytes:   rxB,
			RXPackets: rxP,
			TXBytes:   txB,
			TXPackets: txP,
		})
	}
	return out
}

func (ts *TelemetryServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(ts.collectMetrics())
}

func (ts *TelemetryServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>myos v2.0 Telemetry</title>
<style>
  :root { --bg:#0b1530; --card:#16254a; --border:#2b4180; --text:#e6edf7; --dim:#8fa3c7; --accent:#4c8dff; --ok:#2ecc71; }
  * { box-sizing:border-box; }
  body { margin:0; font-family:ui-monospace,SFMono-Regular,Menlo,monospace; background:var(--bg); color:var(--text); padding:20px; }
  header { display:flex; justify-content:space-between; align-items:center; border-bottom:1px solid var(--border); padding-bottom:12px; margin-bottom:20px; }
  h1 { margin:0; font-size:20px; color:var(--accent); }
  .badge { background:#1c3d2e; color:var(--ok); padding:4px 10px; border-radius:4px; font-size:12px; border:1px solid var(--ok); }
  .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(300px,1fr)); gap:16px; margin-bottom:16px; }
  .card { background:var(--card); border:1px solid var(--border); border-radius:6px; padding:16px; }
  .card h2 { margin:0 0 12px 0; font-size:14px; color:var(--accent); text-transform:uppercase; letter-spacing:1px; }
  .bar-bg { background:#0b1530; border:1px solid var(--border); height:16px; border-radius:3px; overflow:hidden; margin:6px 0 12px 0; }
  .bar-fill { height:100%; background:var(--accent); transition:width 0.3s ease; }
  .bar-fill.mem { background:var(--ok); }
  .row { display:flex; justify-content:space-between; font-size:13px; margin:4px 0; }
  .dim { color:var(--dim); }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th, td { text-align:left; padding:6px 8px; border-bottom:1px solid var(--border); }
  th { color:var(--dim); font-weight:normal; }
</style>
</head>
<body>
<header>
  <h1>myos v2.0 // Live Telemetry</h1>
  <div><span class="dim" id="meta">uptime: - | procs: -</span> <span class="badge" id="status">LIVE</span></div>
</header>
<div class="grid">
  <div class="card">
    <h2>CPU &amp; Memory</h2>
    <div class="row"><span>CPU Utilization</span><span id="cpu-val">0.0%</span></div>
    <div class="bar-bg"><div class="bar-fill" id="cpu-bar" style="width:0%"></div></div>
    <div class="row"><span>Memory Usage</span><span id="mem-val">0 / 0 MB</span></div>
    <div class="bar-bg"><div class="bar-fill mem" id="mem-bar" style="width:0%"></div></div>
    <div class="row dim"><span>Buffers / Cached</span><span id="mem-bc">-</span></div>
  </div>
  <div class="card">
    <h2>Network Interfaces</h2>
    <table><thead><tr><th>Iface</th><th>RX</th><th>TX</th><th>Rate</th></tr></thead><tbody id="net-body"></tbody></table>
  </div>
</div>
<div class="card">
  <h2>Block Devices &amp; Mounts</h2>
  <table><thead><tr><th>Name</th><th>Path</th><th>Type</th><th>Size</th><th>FS</th><th>Mount</th></tr></thead><tbody id="disk-body"></tbody></table>
</div>
<script>
let prevNet = {}, prevTs = 0;
function fmtBytes(b) {
  if (b >= 1073741824) return (b/1073741824).toFixed(1)+'G';
  if (b >= 1048576) return (b/1048576).toFixed(1)+'M';
  if (b >= 1024) return (b/1024).toFixed(1)+'K';
  return b+'B';
}
async function poll() {
  try {
    const r = await fetch('/api/metrics');
    const d = await r.json();
    document.getElementById('status').textContent = 'LIVE';
    document.getElementById('meta').textContent = 'uptime: ' + d.uptime_sec + 's | procs: ' + d.procs;
    const cpu = Math.min(100, Math.max(0, d.cpu_percent || 0));
    document.getElementById('cpu-val').textContent = cpu.toFixed(1) + '%';
    document.getElementById('cpu-bar').style.width = cpu.toFixed(1) + '%';

    const usedMB = Math.round((d.memory.used_kb || 0) / 1024);
    const totalMB = Math.round((d.memory.total_kb || 0) / 1024);
    const memPct = totalMB > 0 ? (usedMB * 100 / totalMB) : 0;
    document.getElementById('mem-val').textContent = usedMB + ' / ' + totalMB + ' MB';
    document.getElementById('mem-bar').style.width = memPct.toFixed(1) + '%';
    document.getElementById('mem-bc').textContent = Math.round((d.memory.buffers_kb||0)/1024) + 'M / ' + Math.round((d.memory.cached_kb||0)/1024) + 'M';

    const dt = prevTs ? Math.max(1, d.timestamp - prevTs) : 1;
    let netHtml = '';
    for (const n of (d.net || [])) {
      const p = prevNet[n.name];
      let rate = '-';
      if (p) {
        const dr = Math.max(0, (n.rx_bytes - p.rx_bytes) + (n.tx_bytes - p.tx_bytes)) / dt;
        rate = fmtBytes(Math.round(dr)) + '/s';
      }
      prevNet[n.name] = n;
      netHtml += '<tr><td>'+n.name+'</td><td>'+fmtBytes(n.rx_bytes)+'</td><td>'+fmtBytes(n.tx_bytes)+'</td><td>'+rate+'</td></tr>';
    }
    prevTs = d.timestamp;
    document.getElementById('net-body').innerHTML = netHtml || '<tr><td colspan="4" class="dim">no interfaces</td></tr>';

    let diskHtml = '';
    for (const k of (d.disks || [])) {
      diskHtml += '<tr><td>'+k.Name+'</td><td>'+k.Path+'</td><td>'+k.Type+'</td><td>'+k.SizeHuman+'</td><td>'+k.FSType+'</td><td>'+k.Mount+'</td></tr>';
    }
    document.getElementById('disk-body').innerHTML = diskHtml || '<tr><td colspan="6" class="dim">no block devices</td></tr>';
  } catch (e) {
    document.getElementById('status').textContent = 'OFFLINE';
  }
}
poll();
setInterval(poll, 1000);
</script>
</body>
</html>`
