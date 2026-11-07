// Package proc spawns child processes and reaps zombies in a way that is
// safe for PID 1: a single reaper owns wait4(-1), and children started via
// Spawn get their exit status delivered through it (os/exec would race
// with the reaper and see ECHILD).
package proc

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	mu        sync.Mutex
	startOnce sync.Once
	waiters   = map[int]chan syscall.WaitStatus{}
	exited    = map[int]syscall.WaitStatus{} // reaped before Wait was called
)

// StartReaper reaps every terminated child on SIGCHLD (plus a 1s safety tick
// in case signals coalesce).
func StartReaper() {
	startOnce.Do(func() {
		ch := make(chan os.Signal, 16)
		signal.Notify(ch, syscall.SIGCHLD)
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ch:
				case <-t.C:
				}
				reap()
			}
		}()
	})
}

func reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		mu.Lock()
		if c, ok := waiters[pid]; ok {
			delete(waiters, pid)
			c <- ws // buffered
		} else {
			if len(exited) > 1024 { // orphans nobody will claim
				for k := range exited {
					delete(exited, k)
					break
				}
			}
			exited[pid] = ws
		}
		mu.Unlock()
	}
}

// Spawn starts path with argv and the given stdio file descriptors.
func Spawn(path string, argv, env []string, files []uintptr) (int, error) {
	StartReaper()
	dir, _ := os.Getwd()
	return syscall.ForkExec(path, argv, &syscall.ProcAttr{Dir: dir, Env: env, Files: files})
}

// Wait blocks until the child pid (started by Spawn) exits.
func Wait(pid int) syscall.WaitStatus {
	if pid <= 0 {
		return 0
	}
	StartReaper()
	mu.Lock()
	if ws, ok := exited[pid]; ok {
		delete(exited, pid)
		mu.Unlock()
		return ws
	}
	c := make(chan syscall.WaitStatus, 1)
	waiters[pid] = c
	mu.Unlock()
	return <-c
}
