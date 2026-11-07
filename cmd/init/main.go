// Command init is PID 1 of the pure-Go myos distribution.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"myos/internal/input"
	"myos/internal/netcfg"
	"myos/internal/proc"
	"myos/internal/shell"
	"myos/internal/sys"
)

func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, "[init] "+format+"\n", a...) }

func main() {
	log.SetFlags(0)
	pid1 := os.Getpid() == 1
	if pid1 {
		if err := sys.MountEarly(); err != nil {
			logf("mount: %v", err)
		}
		sys.QuietKernel()
		// The kernel's /dev/console follows the last console= argument; allow
		// choosing the shell terminal explicitly (myos.tty=/dev/tty0).
		if err := sys.AttachConsole(sys.CmdlineValue("myos.tty")); err != nil {
			logf("console: %v", err)
		}
		_ = os.Setenv("PATH", "/bin:/sbin:/usr/bin:/usr/sbin")
		_ = os.Setenv("HOME", "/")
		if os.Getenv("TERM") == "" {
			_ = os.Setenv("TERM", "linux")
		}
		for _, err := range sys.LoadModules("/etc/modules") {
			logf("module: %v", err)
		}
	} else {
		logf("not PID 1: running in test mode (no mounts/network setup)")
	}

	// PID 1 must survive: swallow SIGINT/SIGTERM (a kernel panic follows if
	// init dies) and never die from SIGPIPE. SIGCHLD is handled by the reaper.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	go func() {
		for s := range sigs {
			if s == syscall.SIGINT || s == syscall.SIGTERM {
				logf("ignoring %v (use poweroff/reboot)", s)
			}
		}
	}()
	proc.StartReaper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mouse := input.New("/dev/input/mice", 1024, 768)
	go mouse.Run(ctx)

	if pid1 {
		if err := netcfg.Loopback(); err != nil {
			logf("loopback: %v", err)
		}
		go bringUpNetwork()
	}

	for {
		runShell(mouse)
		if !pid1 {
			return
		}
		fmt.Fprintln(os.Stderr, "[init] shell exited; respawning")
		time.Sleep(200 * time.Millisecond)
	}
}

// runShell runs the shell and recovers from panics so PID 1 never exits.
func runShell(m *input.Mouse) {
	defer func() {
		if r := recover(); r != nil {
			logf("shell panic: %v", r)
		}
	}()
	shell.New(m).Run()
}

func bringUpNetwork() {
	ifc, err := netcfg.PrimaryInterface(10 * time.Second)
	if err != nil {
		logf("net: %v", err)
		return
	}
	logf("net: using %s (%s)", ifc.Name, ifc.HardwareAddr)
	lease, err := netcfg.Configure(ifc, logf)
	if err != nil {
		logf("net: %v", err)
		return
	}
	logf("net: %s leased %s/%d gw %v dns %v", ifc.Name, lease.IP, lease.PrefixLen(), lease.Router, lease.DNS)
}
