package proc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestSpawnAndWaitExitSuccess(t *testing.T) {
	sh, err := exec.LookPath("true")
	if err != nil {
		sh = "/bin/true"
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	pid, err := Spawn(sh, []string{"true"}, os.Environ(), []uintptr{r.Fd(), w.Fd(), w.Fd()})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("unexpected pid: %d", pid)
	}
	ws := Wait(pid)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("expected exit status 0, got %v (status=%d)", ws, ws.ExitStatus())
	}
}

func TestSpawnAndWaitExitFailure(t *testing.T) {
	sh, err := exec.LookPath("false")
	if err != nil {
		sh = "/bin/false"
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	pid, err := Spawn(sh, []string{"false"}, os.Environ(), []uintptr{r.Fd(), w.Fd(), w.Fd()})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	ws := Wait(pid)
	if !ws.Exited() || ws.ExitStatus() != 1 {
		t.Fatalf("expected exit status 1, got %v (status=%d)", ws, ws.ExitStatus())
	}
}

func TestWaitInvalidPid(t *testing.T) {
	ws := Wait(-1)
	if ws != 0 {
		t.Fatalf("expected 0 for invalid pid, got %v", ws)
	}
	ws0 := Wait(0)
	if ws0 != 0 {
		t.Fatalf("expected 0 for pid 0, got %v", ws0)
	}
}

func TestMultipleConcurrentChildren(t *testing.T) {
	sh, err := exec.LookPath("sleep")
	if err != nil {
		sh = "/bin/sleep"
	}
	pids := make([]int, 5)
	for i := 0; i < len(pids); i++ {
		pid, err := Spawn(sh, []string{"sleep", "0.05"}, os.Environ(), []uintptr{0, 1, 2})
		if err != nil {
			t.Fatalf("Spawn %d failed: %v", i, err)
		}
		pids[i] = pid
	}
	for i, pid := range pids {
		ws := Wait(pid)
		if !ws.Exited() || ws.ExitStatus() != 0 {
			t.Fatalf("child %d (pid %d) failed: %v", i, pid, ws)
		}
	}
}

func TestWaitChildAlreadyExited(t *testing.T) {
	sh, err := exec.LookPath("true")
	if err != nil {
		sh = "/bin/true"
	}
	pid, err := Spawn(sh, []string{"true"}, os.Environ(), []uintptr{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	// Give the child and reaper plenty of time to reap the process first
	time.Sleep(100 * time.Millisecond)
	ws := Wait(pid)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("expected exit 0 for already reaped child, got %v", ws)
	}
}
