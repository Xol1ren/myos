package shell

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"myos/internal/fb"
	"myos/internal/input"
	"myos/internal/monitor"
	"myos/internal/netcfg"
	"myos/internal/sys"
)

// IO is the stdio of a builtin (may be pipes or redirected files).
type IO struct {
	In       io.Reader
	Out, Err io.Writer
}

type builtinFn func(s *Shell, io *IO, args []string) int

var builtins map[string]builtinFn

func init() {
	builtins = map[string]builtinFn{
		"ls":       biLs,
		"cat":      biCat,
		"ip":       biIP,
		"ping":     biPing,
		"mouse":    biMouse,
		"poweroff": func(s *Shell, io *IO, a []string) int { return biHalt(io, syscall.LINUX_REBOOT_CMD_POWER_OFF) },
		"reboot":   func(s *Shell, io *IO, a []string) int { return biHalt(io, syscall.LINUX_REBOOT_CMD_RESTART) },
		"cd":       biCd,
		"pwd":      biPwd,
		"echo":     biEcho,
		"clear":    func(s *Shell, io *IO, a []string) int { fmt.Fprint(io.Out, "\x1b[H\x1b[2J"); return 0 },
		"history":  biHistory,
		"help":     biHelp,
		"syshud":   biSyshud,
		"gui":      biGui,
		"disks":    biDisks,
		"fetch":    biFetch,
	}
}

func biHelp(_ *Shell, io *IO, _ []string) int {
	names := make([]string, 0, len(builtins)+1)
	for n := range builtins {
		names = append(names, n)
	}
	names = append(names, "exit")
	sort.Strings(names)
	fmt.Fprintln(io.Out, "builtins: "+strings.Join(names, " "))
	fmt.Fprintln(io.Out, "syntax:   cmd | cmd, cmd > file, cmd >> file, cmd < file, 'quotes', $VAR, $?")
	fmt.Fprintln(io.Out, "editing:  arrows, Home/End, Ctrl-A/E/K/U/W/L, Ctrl-C, Ctrl-D")
	return 0
}

func biEcho(_ *Shell, io *IO, a []string) int {
	nl := true
	if len(a) > 1 && a[1] == "-n" {
		nl, a = false, a[1:]
	}
	fmt.Fprint(io.Out, strings.Join(a[1:], " "))
	if nl {
		fmt.Fprintln(io.Out)
	}
	return 0
}

func biPwd(_ *Shell, io *IO, _ []string) int {
	d, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(io.Err, "pwd:", err)
		return 1
	}
	fmt.Fprintln(io.Out, d)
	return 0
}

func biCd(_ *Shell, io *IO, a []string) int {
	dir := "/"
	if len(a) > 1 {
		dir = a[1]
	} else if h := os.Getenv("HOME"); h != "" {
		dir = h
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(io.Err, "cd:", err)
		return 1
	}
	return 0
}

func biHistory(s *Shell, io *IO, _ []string) int {
	for i, h := range s.history {
		fmt.Fprintf(io.Out, "%4d  %s\n", i+1, h)
	}
	return 0
}

func biCat(_ *Shell, io *IO, a []string) int {
	if len(a) == 1 {
		_, _ = io2copy(io.Out, io.In)
		return 0
	}
	rc := 0
	for _, p := range a[1:] {
		if p == "-" {
			_, _ = io2copy(io.Out, io.In)
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(io.Err, "cat:", err)
			rc = 1
			continue
		}
		if st, err := f.Stat(); err == nil && st.IsDir() {
			fmt.Fprintf(io.Err, "cat: %s: Is a directory\n", p)
			rc = 1
		} else if _, err := io2copy(io.Out, f); err != nil {
			fmt.Fprintln(io.Err, "cat:", err)
			rc = 1
		}
		f.Close()
	}
	return rc
}

func io2copy(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

func biLs(_ *Shell, io *IO, a []string) int {
	var paths []string
	for _, x := range a[1:] {
		if !strings.HasPrefix(x, "-") { // flags accepted and ignored: output is always long
			paths = append(paths, x)
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	rc := 0
	for i, p := range paths {
		st, err := os.Lstat(p)
		if err != nil {
			fmt.Fprintln(io.Err, "ls:", err)
			rc = 1
			continue
		}
		if len(paths) > 1 {
			if i > 0 {
				fmt.Fprintln(io.Out)
			}
			fmt.Fprintf(io.Out, "%s:\n", p)
		}
		if !st.IsDir() {
			printEntry(io.Out, p, st)
			continue
		}
		ents, err := os.ReadDir(p)
		if err != nil {
			fmt.Fprintln(io.Err, "ls:", err)
			rc = 1
			continue
		}
		for _, e := range ents {
			info, err := e.Info()
			if err != nil {
				continue
			}
			printEntry(io.Out, filepath.Join(p, e.Name()), info)
		}
	}
	return rc
}

func printEntry(w io.Writer, full string, st os.FileInfo) {
	name := st.Name()
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		if t, err := os.Readlink(full); err == nil {
			name += " -> " + t
		}
	case st.IsDir():
		name += "/"
	}
	fmt.Fprintf(w, "%s %10d %s %s\n", st.Mode(), st.Size(), st.ModTime().Format("Jan _2 15:04"), name)
}

func biIP(_ *Shell, io *IO, _ []string) int {
	ifs, err := net.Interfaces()
	if err != nil {
		fmt.Fprintln(io.Err, "ip:", err)
		return 1
	}
	for _, ifc := range ifs {
		fmt.Fprintf(io.Out, "%d: %s: <%s> mtu %d\n", ifc.Index, ifc.Name, strings.ToUpper(strings.ReplaceAll(ifc.Flags.String(), "|", ",")), ifc.MTU)
		if len(ifc.HardwareAddr) > 0 {
			fmt.Fprintf(io.Out, "    link/ether %s\n", ifc.HardwareAddr)
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			fam := "inet"
			if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() == nil {
				fam = "inet6"
			}
			fmt.Fprintf(io.Out, "    %s %s\n", fam, ad)
		}
	}
	if gw := defaultGateway(); gw != "" {
		fmt.Fprintf(io.Out, "default via %s\n", gw)
	}
	return 0
}

// defaultGateway reads the IPv4 default route from /proc/net/route.
func defaultGateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) > 2 && f[1] == "00000000" {
			v, err := strconv.ParseUint(f[2], 16, 32)
			if err != nil {
				continue
			}
			var ip [4]byte
			binary.NativeEndian.PutUint32(ip[:], uint32(v))
			return net.IP(ip[:]).String() + " dev " + f[0]
		}
	}
	return ""
}

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func biPing(_ *Shell, io *IO, a []string) int {
	count := 4
	var host string
	for i := 1; i < len(a); i++ {
		if a[i] == "-c" && i+1 < len(a) {
			n, err := strconv.Atoi(a[i+1])
			if err != nil || n < 1 {
				fmt.Fprintln(io.Err, "ping: bad count")
				return 2
			}
			count = n
			i++
		} else {
			host = a[i]
		}
	}
	if host == "" {
		fmt.Fprintln(io.Err, "usage: ping [-c count] host")
		return 2
	}
	dst, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		fmt.Fprintf(io.Err, "ping: %s: %v\n", host, err)
		return 1
	}
	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		fmt.Fprintln(io.Err, "ping: raw socket:", err)
		return 1
	}
	defer conn.Close()

	id := uint16(os.Getpid())
	payload := []byte("myos-ping-payload-0123456789abcd")
	fmt.Fprintf(io.Out, "PING %s (%s) %d bytes of data\n", host, dst.IP, len(payload))
	var sent, got int
	var minRTT, maxRTT, sumRTT time.Duration
	buf := make([]byte, 1500)
	for seq := 1; seq <= count; seq++ {
		msg := make([]byte, 8+len(payload))
		msg[0] = 8
		binary.BigEndian.PutUint16(msg[4:], id)
		binary.BigEndian.PutUint16(msg[6:], uint16(seq))
		copy(msg[8:], payload)
		binary.BigEndian.PutUint16(msg[2:], icmpChecksum(msg))

		start := time.Now()
		if _, err := conn.WriteTo(msg, dst); err != nil {
			fmt.Fprintln(io.Err, "ping: send:", err)
			break
		}
		sent++
		_ = conn.SetReadDeadline(start.Add(2 * time.Second))
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				fmt.Fprintf(io.Out, "Request timeout for icmp_seq %d\n", seq)
				break
			}
			icmp := buf[:n]
			if n >= 20 && buf[0]>>4 == 4 { // IPv4 header present on raw socket
				ihl := int(buf[0]&0x0f) * 4
				if n >= ihl+8 {
					icmp = buf[ihl:n]
				}
			}
			if len(icmp) < 8 {
				continue
			}
			if icmp[0] == 0 && binary.BigEndian.Uint16(icmp[4:]) == id && binary.BigEndian.Uint16(icmp[6:]) == uint16(seq) {
				rtt := time.Since(start)
				got++
				sumRTT += rtt
				if got == 1 || rtt < minRTT {
					minRTT = rtt
				}
				if rtt > maxRTT {
					maxRTT = rtt
				}
				fmt.Fprintf(io.Out, "%d bytes from %s: icmp_seq=%d time=%.2f ms\n", len(icmp), from, seq, float64(rtt.Microseconds())/1000)
				break
			}
			if icmp[0] == 3 { // destination unreachable
				fmt.Fprintf(io.Out, "From %s: destination unreachable (code %d)\n", from, icmp[1])
				break
			}
		}
		if seq < count {
			time.Sleep(time.Until(start.Add(time.Second)))
		}
	}
	loss := 100.0
	if sent > 0 {
		loss = float64(sent-got) * 100 / float64(sent)
	}
	fmt.Fprintf(io.Out, "--- %s ping statistics ---\n%d transmitted, %d received, %.0f%% loss\n", host, sent, got, loss)
	if got > 0 {
		fmt.Fprintf(io.Out, "rtt min/avg/max = %.2f/%.2f/%.2f ms\n",
			float64(minRTT.Microseconds())/1000, float64(sumRTT.Microseconds())/1000/float64(got), float64(maxRTT.Microseconds())/1000)
	}
	if got == 0 {
		return 1
	}
	return 0
}

func btnString(b uint8) string {
	var parts []string
	if b&input.Left != 0 {
		parts = append(parts, "L")
	}
	if b&input.Middle != 0 {
		parts = append(parts, "M")
	}
	if b&input.Right != 0 {
		parts = append(parts, "R")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "+")
}

// biMouse shows live mouse state until 'q' is pressed. `mouse -1` prints a snapshot.
func biMouse(s *Shell, io *IO, a []string) int {
	if s.Mouse == nil {
		fmt.Fprintln(io.Err, "mouse: no mouse tracker")
		return 1
	}
	st := s.Mouse.Snapshot()
	if (len(a) > 1 && a[1] == "-1") || !s.tty {
		fmt.Fprintf(io.Out, "x=%d y=%d buttons=%s packets=%d connected=%v (screen %dx%d)\n",
			st.X, st.Y, btnString(st.Buttons), st.Packets, st.Connected, st.W, st.H)
		return 0
	}
	if !st.Connected {
		fmt.Fprintln(io.Out, "(waiting for /dev/input/mice; is a mouse attached and mousedev loaded?)")
	}
	fmt.Fprintln(io.Out, "mouse: move/click to see events, press q or Ctrl-C to quit")
	restore, err := makeRaw(0)
	if err != nil {
		fmt.Fprintln(io.Err, "mouse:", err)
		return 1
	}
	defer restore()

	quit := make(chan struct{})
	go func() {
		b := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(b); err != nil || b[0] == 'q' || b[0] == 3 {
				close(quit)
				return
			}
		}
	}()
	evs := s.Mouse.Subscribe()
	defer s.Mouse.Unsubscribe(evs)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-quit:
			fmt.Fprint(io.Out, "\r\n")
			return 0
		case ev := <-evs:
			if ev.Changed != 0 {
				for _, bit := range []uint8{input.Left, input.Middle, input.Right} {
					if ev.Changed&bit != 0 {
						state := "released"
						if ev.Buttons&bit != 0 {
							state = "pressed"
						}
						fmt.Fprintf(io.Out, "\r\x1b[K%s button %s at (%d,%d)\r\n", btnString(bit), state, ev.X, ev.Y)
					}
				}
			}
		case <-tick.C:
			st := s.Mouse.Snapshot()
			fmt.Fprintf(io.Out, "\r\x1b[Kx=%-5d y=%-5d buttons=%-5s packets=%d", st.X, st.Y, btnString(st.Buttons), st.Packets)
		}
	}
}

func biHalt(io *IO, cmd int) int {
	what := "Power off"
	if cmd == syscall.LINUX_REBOOT_CMD_RESTART {
		what = "Rebooting"
	}
	fmt.Fprintf(io.Out, "%s...\n", what)
	if err := sys.Shutdown(cmd); err != nil {
		fmt.Fprintln(io.Err, "shutdown:", err)
		return 1
	}
	return 0
}

// biSyshud runs the live system monitor: syshud [iface].
func biSyshud(s *Shell, io *IO, a []string) int {
	if !s.tty {
		fmt.Fprintln(io.Err, "syshud: needs an interactive terminal")
		return 1
	}
	out, ok := io.Out.(*os.File)
	if !ok {
		fmt.Fprintln(io.Err, "syshud: output must be a terminal")
		return 1
	}
	iface := "eth0"
	if len(a) > 1 {
		iface = a[1]
	}
	restore, err := makeRaw(0)
	if err != nil {
		fmt.Fprintln(io.Err, "syshud:", err)
		return 1
	}
	defer restore()
	if err := monitor.Run(out, iface, time.Second, waitQuit); err != nil {
		fmt.Fprintln(io.Err, "syshud:", err)
		return 1
	}
	return 0
}

// biGui runs the desktop until Esc or q: gui [device].
func biGui(s *Shell, io *IO, a []string) int {
	if !s.tty {
		fmt.Fprintln(io.Err, "gui: needs an interactive terminal")
		return 1
	}
	if s.Mouse == nil {
		fmt.Fprintln(io.Err, "gui: no mouse tracker")
		return 1
	}
	preferred := ""
	if len(a) > 1 {
		preferred = a[1]
	}
	dev, err := fb.OpenDevice(preferred)
	if err != nil {
		fmt.Fprintf(io.Err, "gui: %v\n", err)
		return 1
	}
	defer dev.Close()
	restoreTTY, err := makeRaw(0)
	if err != nil {
		fmt.Fprintln(io.Err, "gui:", err)
		return 1
	}
	restoreVT := fb.GraphicsMode(0)
	// Deferred in this order: close the GUI, back to text mode, cooked tty,
	// then repaint a clean console.
	defer func() {
		restoreVT()
		restoreTTY()
		fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	}()

	// Dual Keyboard Listener: multiplexes os.Stdin and /dev/tty0
	klist, _ := fb.OpenDualKeyboard()
	if klist != nil {
		defer klist.Close()
	}

	desk := fb.NewDesktop(dev.Width(), dev.Height())
	guiWait := func(d time.Duration) bool {
		deadline := time.Now().Add(d)
		for {
			rem := time.Until(deadline)
			if rem <= 0 {
				return false
			}
			var data []byte
			var ok, eof bool
			if klist != nil {
				data, ok, eof = klist.Poll(rem)
			} else {
				var k byte
				k, ok, eof = pollKey(rem)
				if ok {
					data = []byte{k}
				}
			}
			if eof {
				return true
			}
			if !ok {
				return false
			}
			if desk.RouteInput(data) {
				return true
			}
		}
	}
	desk.Run(dev, s.Mouse, guiWait)
	return 0
}

// biDisks scans and reports block storage devices: disks
func biDisks(_ *Shell, io *IO, _ []string) int {
	disks, err := sys.ScanDisks()
	if err != nil {
		fmt.Fprintf(io.Err, "disks: %v\n", err)
		return 1
	}
	if len(disks) == 0 {
		fmt.Fprintln(io.Out, "No block storage devices detected (/dev/sd*, /dev/vd*, /dev/nvme*)")
		return 0
	}
	fmt.Fprintf(io.Out, "%-14s %-10s %-8s %-16s %s\n", "DEVICE", "SIZE", "TYPE", "MOUNTPOINT", "FSTYPE")
	for _, d := range disks {
		fmt.Fprintf(io.Out, "%-14s %-10s %-8s %-16s %s\n", d.Path, d.SizeHuman, d.Type, d.Mount, d.FSType)
	}
	return 0
}

// biFetch downloads files over HTTP/HTTPS: fetch <url> [-o output_file]
func biFetch(_ *Shell, io *IO, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(io.Err, "usage: fetch <url> [-o output_file]")
		return 1
	}
	rawURL := args[1]
	outPath := ""
	for i := 2; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			outPath = args[i+1]
			i++
		}
	}

	fmt.Fprintf(io.Err, "fetching %s ...\n", rawURL)
	start := time.Now()
	res, err := netcfg.Fetch(context.Background(), rawURL, netcfg.FetchOptions{
		Timeout: 30 * time.Second,
		Out:     io.Out,
		OutPath: outPath,
		Progress: func(downloaded, total int64, elapsed time.Duration, done bool) {
			if total > 0 {
				pct := (downloaded * 100) / total
				fmt.Fprintf(io.Err, "\r[%-20s] %d%% %s / %s", strings.Repeat("=", int(pct/5))+">", pct, sys.FormatBytes(uint64(downloaded)), sys.FormatBytes(uint64(total)))
			} else {
				fmt.Fprintf(io.Err, "\r%s downloaded", sys.FormatBytes(uint64(downloaded)))
			}
			if done {
				fmt.Fprintln(io.Err)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(io.Err, "fetch: %v\n", err)
		return 1
	}

	rate := float64(res.BytesRead) / (time.Since(start).Seconds() + 0.001)
	dest := "stdout"
	if outPath != "" {
		dest = outPath
	}
	fmt.Fprintf(io.Err, "downloaded %d bytes in %v (%s/s) -> %s (HTTP %d %s)\n",
		res.BytesRead, res.Duration.Round(time.Millisecond), sys.FormatBytes(uint64(rate)), dest, res.StatusCode, res.StatusText)
	return 0
}

