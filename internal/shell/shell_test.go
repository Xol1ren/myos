package shell

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBasic(t *testing.T) {
	lookup := func(k string) string {
		if k == "FOO" {
			return "bar"
		}
		if k == "?" {
			return "0"
		}
		return ""
	}

	stages, err := parse("echo hello world", lookup)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stages) != 1 || len(stages[0].args) != 3 || stages[0].args[0] != "echo" {
		t.Fatalf("stages: %+v", stages)
	}
}

func TestParsePipesAndRedirection(t *testing.T) {
	lookup := func(string) string { return "" }

	stages, err := parse("cat < in.txt | grep foo > out.txt", lookup)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}
	if stages[0].args[0] != "cat" || stages[0].in != "in.txt" {
		t.Errorf("stage 0: %+v", stages[0])
	}
	if stages[1].args[0] != "grep" || stages[1].args[1] != "foo" || stages[1].out != "out.txt" || stages[1].appendOut {
		t.Errorf("stage 1: %+v", stages[1])
	}

	stages, err = parse("echo a >> out.txt", lookup)
	if err != nil || len(stages) != 1 || stages[0].out != "out.txt" || !stages[0].appendOut {
		t.Fatalf("append redirection failed: %+v %v", stages, err)
	}
}

func TestParseVariableExpansion(t *testing.T) {
	lookup := func(k string) string {
		switch k {
		case "NAME":
			return "myos"
		case "VER":
			return "1.0"
		case "?":
			return "42"
		default:
			return ""
		}
	}

	stages, err := parse("echo $NAME ${NAME}_${VER} exit=$?", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || len(stages[0].args) != 4 {
		t.Fatalf("stages: %+v", stages)
	}
	if stages[0].args[1] != "myos" {
		t.Errorf("got %q, want myos", stages[0].args[1])
	}
	if stages[0].args[2] != "myos_1.0" {
		t.Errorf("got %q, want myos_1.0", stages[0].args[2])
	}
	if stages[0].args[3] != "exit=42" {
		t.Errorf("got %q, want exit=42", stages[0].args[3])
	}
}

func TestParseAssignments(t *testing.T) {
	lookup := func(string) string { return "" }

	// Assignment only line
	stages, err := parse("FOO=bar NUM=123", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || len(stages[0].args) != 0 || len(stages[0].env) != 2 {
		t.Fatalf("assignment only line: %+v", stages)
	}

	// Leading assignment with command
	stages, err = parse("ENV_VAR=xyz echo hi", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || len(stages[0].args) != 2 || len(stages[0].env) != 1 || stages[0].env[0] != "ENV_VAR=xyz" {
		t.Fatalf("leading assignment: %+v", stages)
	}
}

func TestParseSyntaxErrors(t *testing.T) {
	lookup := func(string) string { return "" }
	bad := []string{
		"|",
		"echo a |",
		"| echo a",
		"cat <",
		"echo >",
		"echo 'unterminated",
		"echo \"unterminated",
		"> out.txt",
	}
	for _, s := range bad {
		if _, err := parse(s, lookup); err == nil {
			t.Errorf("expected error for syntax: %q", s)
		}
	}
}

func TestExecuteAssignmentSetsEnv(t *testing.T) {
	s := New(nil)
	t.Setenv("TEST_VAR", "")
	stages, err := parse("TEST_VAR=awesome_value", s.lookup)
	if err != nil {
		t.Fatal(err)
	}
	code := s.execute(stages)
	if code != 0 {
		t.Fatalf("expected code 0, got %d", code)
	}
	if os.Getenv("TEST_VAR") != "awesome_value" {
		t.Fatalf("env not set, got %q", os.Getenv("TEST_VAR"))
	}
}

func TestExecutePipelineAndRedirection(t *testing.T) {
	s := New(nil)
	tmp := filepath.Join(t.TempDir(), "test.txt")

	// Write to file via redirection
	stages, err := parse("echo hello world > "+tmp, s.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if code := s.execute(stages); code != 0 {
		t.Fatalf("execute failed: %d", code)
	}

	content, err := os.ReadFile(tmp)
	if err != nil || strings.TrimSpace(string(content)) != "hello world" {
		t.Fatalf("file content: %q, err: %v", string(content), err)
	}

	// Append to file
	stages, err = parse("echo second line >> "+tmp, s.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if code := s.execute(stages); code != 0 {
		t.Fatalf("append failed: %d", code)
	}

	// Read via cat and pipeline
	tmpOut := filepath.Join(t.TempDir(), "copy.txt")
	stages, err = parse("cat < "+tmp+" > "+tmpOut, s.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if code := s.execute(stages); code != 0 {
		t.Fatalf("copy failed: %d", code)
	}

	content2, err := os.ReadFile(tmpOut)
	if err != nil || string(content2) != string(content)+"second line\n" {
		t.Fatalf("copy content mismatch: %q", string(content2))
	}
}

func TestExecuteGlobbing(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.log"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.log"), []byte("2"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte("3"), 0o644)

	s := New(nil)
	tmpOut := filepath.Join(t.TempDir(), "glob.txt")
	stages, err := parse("echo "+filepath.Join(dir, "*.log")+" > "+tmpOut, s.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if code := s.execute(stages); code != 0 {
		t.Fatalf("execute failed: %d", code)
	}
	b, err := os.ReadFile(tmpOut)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(string(b))
	wantA := filepath.Join(dir, "a.log")
	wantB := filepath.Join(dir, "b.log")
	if !strings.Contains(out, wantA) || !strings.Contains(out, wantB) || strings.Contains(out, "c.txt") {
		t.Fatalf("glob output mismatch: %q", out)
	}
}

func TestBuiltinHelpPwdCdEchoCat(t *testing.T) {
	s := New(nil)

	// echo
	var buf bytes.Buffer
	ioEcho := &IO{Out: &buf, Err: &buf}
	if biEcho(s, ioEcho, []string{"echo", "hi"}) != 0 || buf.String() != "hi\n" {
		t.Fatalf("echo: %q", buf.String())
	}

	// echo -n
	buf.Reset()
	if biEcho(s, ioEcho, []string{"echo", "-n", "hi"}) != 0 || buf.String() != "hi" {
		t.Fatalf("echo -n: %q", buf.String())
	}

	// pwd
	buf.Reset()
	ioPwd := &IO{Out: &buf, Err: &buf}
	if biPwd(s, ioPwd, []string{"pwd"}) != 0 || !strings.HasPrefix(buf.String(), "/") {
		t.Fatalf("pwd: %q", buf.String())
	}

	// help
	buf.Reset()
	ioHelp := &IO{Out: &buf, Err: &buf}
	if biHelp(s, ioHelp, []string{"help"}) != 0 || !strings.Contains(buf.String(), "builtins:") {
		t.Fatalf("help: %q", buf.String())
	}

	// cat with -
	buf.Reset()
	ioCat := &IO{In: strings.NewReader("piped input\n"), Out: &buf, Err: &buf}
	if biCat(s, ioCat, []string{"cat", "-"}) != 0 || buf.String() != "piped input\n" {
		t.Fatalf("cat -: %q", buf.String())
	}
}

func TestPollKeyTimeout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	// 1. Timeout on empty pipe
	start := time.Now()
	k, ok, eof := pollKeyFd(int(r.Fd()), 50*time.Millisecond)
	dur := time.Since(start)
	if ok || eof || k != 0 {
		t.Fatalf("pollKeyFd expected timeout, got k=%d ok=%v eof=%v", k, ok, eof)
	}
	if dur < 30*time.Millisecond {
		t.Fatalf("pollKeyFd returned too quickly: %v", dur)
	}

	// 2. Data available
	_, _ = w.Write([]byte{'q'})
	k, ok, eof = pollKeyFd(int(r.Fd()), 50*time.Millisecond)
	if !ok || eof || k != 'q' {
		t.Fatalf("expected 'q', got k=%c ok=%v eof=%v", k, ok, eof)
	}

	// 3. EOF on closed writer
	_ = w.Close()
	k, ok, eof = pollKeyFd(int(r.Fd()), 50*time.Millisecond)
	if ok || !eof {
		t.Fatalf("expected eof, got k=%d ok=%v eof=%v", k, ok, eof)
	}
}
