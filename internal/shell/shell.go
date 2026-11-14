// Package shell implements the interactive myos shell and its built-in
// coreutils.
package shell

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"myos/internal/fb"
	"myos/internal/input"
	"myos/internal/proc"
	"myos/internal/sys"
)

const ioctlTIOCSPGRP = 0x5410

// Shell is the interactive REPL state.
type Shell struct {
	Prompt string
	Mouse  *input.Mouse

	in      *os.File
	out     *os.File
	err     *os.File
	stdin   *bufio.Reader
	tty     bool
	history []string
	status  int
}

func init() {
	fb.ShellWorker = func(slave *os.File) {
		RunAttached(slave)
	}
}

// New creates a shell bound to the process's stdin/stdout.
func New(m *input.Mouse) *Shell {
	return NewWithIO(m, os.Stdin, os.Stdout, os.Stderr)
}

// NewWithIO creates a shell with custom input/output descriptors (e.g. PTY slave).
func NewWithIO(m *input.Mouse, in, out, err *os.File) *Shell {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if err == nil {
		err = os.Stderr
	}
	return &Shell{
		Prompt: "myos# ",
		Mouse:  m,
		in:     in,
		out:    out,
		err:    err,
		stdin:  bufio.NewReader(in),
		tty:    isTTY(int(in.Fd())),
	}
}

// RunAttached runs an interactive shell attached to a given PTY slave or duplex file.
func RunAttached(f *os.File) {
	s := NewWithIO(nil, f, f, f)
	s.Run()
}

func (s *Shell) inFile() *os.File {
	if s.in != nil {
		return s.in
	}
	return os.Stdin
}

func (s *Shell) outFile() *os.File {
	if s.out != nil {
		return s.out
	}
	return os.Stdout
}

func (s *Shell) errFile() *os.File {
	if s.err != nil {
		return s.err
	}
	return os.Stderr
}

// Run executes the REPL until EOF (Ctrl-D) or the `exit` builtin.
func (s *Shell) Run() {
	if s.in == nil {
		s.in = os.Stdin
	}
	if s.out == nil {
		s.out = os.Stdout
	}
	if s.err == nil {
		s.err = os.Stderr
	}
	if s.stdin == nil {
		s.stdin = bufio.NewReader(s.in)
	}
	s.tty = isTTY(int(s.in.Fd()))
	if s.tty {
		signal.Ignore(syscall.SIGTTOU, syscall.SIGTTIN)
	}

	for {
		line, eof := s.readLine(s.Prompt)
		if eof {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n := len(s.history); n == 0 || s.history[n-1] != line {
			s.history = append(s.history, line)
		}
		if line == "exit" || strings.HasPrefix(line, "exit ") {
			f := strings.Fields(line)
			if len(f) > 1 {
				if code, err := strconv.Atoi(f[1]); err == nil {
					s.status = code
				}
			}
			return
		}
		stages, err := parse(line, s.lookup)
		if err != nil {
			fmt.Fprintf(s.errFile(), "myos: %v\n", err)
			s.status = 2
			continue
		}
		if len(stages) == 0 {
			continue
		}
		s.status = s.execute(stages)
	}
}

func (s *Shell) lookup(name string) string {
	if name == "?" {
		return strconv.Itoa(s.status)
	}
	return os.Getenv(name)
}

func expandArgs(st stage) []string {
	var out []string
	for i, arg := range st.args {
		if i < len(st.globs) && st.globs[i] {
			matches, err := filepath.Glob(arg)
			if err == nil && len(matches) > 0 {
				sort.Strings(matches)
				out = append(out, matches...)
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func (s *Shell) runStage(st stage, in, out *os.File) int {
	if len(st.args) == 0 {
		for _, e := range st.env {
			k, v, ok := strings.Cut(e, "=")
			if ok {
				_ = os.Setenv(k, v)
			}
		}
		return 0
	}
	args := expandArgs(st)
	if len(args) == 0 {
		return 0
	}
	if b, ok := builtins[args[0]]; ok {
		var stageEnv map[string]string
		if len(st.env) > 0 {
			stageEnv = make(map[string]string, len(st.env))
			for _, e := range st.env {
				if k, v, ok := strings.Cut(e, "="); ok {
					stageEnv[k] = v
				}
			}
		}
		return b(s, &IO{In: in, Out: out, Err: s.errFile(), Env: stageEnv}, args)
	}
	return s.runExternal(args, st.env, in, out)
}

func (s *Shell) runExternal(args []string, extraEnv []string, in, out *os.File) int {
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(s.errFile(), "myos: %s: command not found\n", args[0])
		return 127
	}
	env := os.Environ()
	if len(extraEnv) > 0 {
		env = append(append([]string(nil), env...), extraEnv...)
	}
	sysAttr := &syscall.SysProcAttr{
		Setpgid: true,
	}
	pid, err := proc.SpawnAttr(path, args, env, []uintptr{in.Fd(), out.Fd(), s.errFile().Fd()}, sysAttr)
	if err != nil {
		fmt.Fprintf(s.errFile(), "myos: %s: %v\n", args[0], err)
		return 126
	}
	if s.tty {
		ttyFd := int(s.inFile().Fd())
		shellPgrp := int32(syscall.Getpgrp())
		childPgrp := int32(pid)
		_ = sys.Ioctl(ttyFd, ioctlTIOCSPGRP, unsafe.Pointer(&childPgrp))
		defer func() {
			_ = sys.Ioctl(ttyFd, ioctlTIOCSPGRP, unsafe.Pointer(&shellPgrp))
		}()
	}
	ws := proc.Wait(pid)
	switch {
	case ws.Exited():
		return ws.ExitStatus()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	}
	return 1
}
