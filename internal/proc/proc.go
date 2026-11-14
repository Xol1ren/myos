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
	mu         sync.Mutex
	startOnce  sync.Once
	registered = map[int]bool{}
	waiters    = map[int]chan syscall.WaitStatus{}
	exited     = map[int]syscall.WaitStatus{} // reaped before Wait was called
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
			delete(registered, pid)
			c <- ws // buffered
		} else if registered[pid] {
			exited[pid] = ws
		}
		mu.Unlock()
	}
}

// Spawn starts path with argv and the given stdio file descriptors.
func Spawn(path string, argv, env []string, files []uintptr) (int, error) {
	return SpawnAttr(path, argv, env, files, nil)
}

// SpawnAttr starts path with argv, stdio file descriptors, and optional SysProcAttr.
// The child PID is atomically registered with the reaper before ForkExec unlocks.
func SpawnAttr(path string, argv, env []string, files []uintptr, sys *syscall.SysProcAttr) (int, error) {
	StartReaper()
	dir, _ := os.Getwd()
	mu.Lock()
	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: files,
		Sys:   sys,
	})
	if err == nil && pid > 0 {
		registered[pid] = true
	}
	mu.Unlock()
	return pid, err
}

// Wait blocks until the child pid (started by Spawn/SpawnAttr) exits.
func Wait(pid int) syscall.WaitStatus {
	if pid <= 0 {
		return 0
	}
	StartReaper()
	mu.Lock()
	if ws, ok := exited[pid]; ok {
		delete(exited, pid)
		delete(registered, pid)
		mu.Unlock()
		return ws
	}
	c := make(chan syscall.WaitStatus, 1)
	waiters[pid] = c
	mu.Unlock()
	return <-c
}
