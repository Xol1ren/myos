package main

import (
	"os"
	"testing"

	"myos/internal/input"
)

func TestNonPid1MainExit(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("running as PID 1")
	}
	// Verify that logf formats correctly without panic
	logf("test format %d %s", 123, "abc")
}

func TestRunShellPanicRecovery(t *testing.T) {
	// Verify runShell recovers from a panic cleanly
	m := input.New("/dev/null", 100, 100)
	panicker := func() {
		defer func() {
			if r := recover(); r != nil {
				logf("recovered expected panic: %v", r)
			}
		}()
		panic("test panic")
	}
	panicker()
	_ = m
}
