// Package shell implements the interactive myos shell and its built-in
// coreutils.
package shell

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"myos/internal/input"
	"myos/internal/proc"
)

// Shell is the interactive REPL state.
type Shell struct {
	Prompt string
	Mouse  *input.Mouse

	stdin   *bufio.Reader
	tty     bool
	history []string
	status  int
}

// New creates a shell bound to the process's stdin/stdout.
func New(m *input.Mouse) *Shell {
	return &Shell{Prompt: "myos# ", Mouse: m, stdin: bufio.NewReader(os.Stdin), tty: isTTY(0)}
}

// Run executes the REPL until EOF (Ctrl-D) or the `exit` builtin.
func (s *Shell) Run() {
	s.tty = isTTY(0)
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
			fmt.Fprintf(os.Stderr, "myos: %v\n", err)
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
		if len(st.env) > 0 {
			orig := map[string]string{}
			unset := map[string]bool{}
			for _, e := range st.env {
				k, v, ok := strings.Cut(e, "=")
				if ok {
					if old, exists := os.LookupEnv(k); exists {
						orig[k] = old
					} else {
						unset[k] = true
					}
					_ = os.Setenv(k, v)
				}
			}
			defer func() {
				for k, v := range orig {
					_ = os.Setenv(k, v)
				}
				for k := range unset {
					_ = os.Unsetenv(k)
				}
			}()
		}
		return b(s, &IO{In: in, Out: out, Err: os.Stderr}, args)
	}
	return s.runExternal(args, st.env, in, out)
}

func (s *Shell) runExternal(args []string, extraEnv []string, in, out *os.File) int {
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "myos: %s: command not found\n", args[0])
		return 127
	}
	env := os.Environ()
	if len(extraEnv) > 0 {
		env = append(append([]string(nil), env...), extraEnv...)
	}
	pid, err := proc.Spawn(path, args, env, []uintptr{in.Fd(), out.Fd(), os.Stderr.Fd()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "myos: %s: %v\n", args[0], err)
		return 126
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
