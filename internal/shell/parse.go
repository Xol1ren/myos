package shell

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// stage is one command in a pipeline with optional redirections.
type stage struct {
	args      []string
	globs     []bool   // globs[i]: args[i] had an unquoted * ? [ and should be glob-expanded
	lits      []bool   // lits[i]: args[i] contained quoting/escaping (never an assignment)
	env       []string // leading NAME=value assignments (not part of args)
	in, out   string
	appendOut bool
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// extractAssignments moves leading unquoted NAME=value words from args to env.
func (st *stage) extractAssignments() {
	n := 0
	for n < len(st.args) && !st.lits[n] {
		name, _, ok := strings.Cut(st.args[n], "=")
		if !ok || !isIdent(name) {
			break
		}
		st.env = append(st.env, st.args[n])
		n++
	}
	st.args, st.globs, st.lits = st.args[n:], st.globs[n:], st.lits[n:]
}

// parse splits a command line into pipeline stages. It understands quotes,
// backslash escapes, $VAR expansion, '#' comments, |, <, > and >>.
func parse(line string, lookup func(string) string) ([]stage, error) {
	var (
		stages  []stage
		cur     stage
		word    []rune
		inWord  bool
		wGlob   bool
		wLit    bool
		pending int // 0 none, 1 '<', 2 '>', 3 '>>'
	)
	flush := func() error {
		if !inWord {
			return nil
		}
		w := string(word)
		word, inWord = nil, false
		glob, lit := wGlob, wLit
		wGlob, wLit = false, false
		switch pending {
		case 1:
			cur.in = w
		case 2, 3:
			cur.out, cur.appendOut = w, pending == 3
		default:
			cur.args = append(cur.args, w)
			cur.globs = append(cur.globs, glob)
			cur.lits = append(cur.lits, lit)
		}
		pending = 0
		return nil
	}
	expand := func(rs []rune, i int) (string, int) { // rs[i] == '$'
		j := i + 1
		if j < len(rs) && rs[j] == '?' {
			return lookup("?"), j + 1
		}
		if j < len(rs) && rs[j] == '{' {
			end := j + 1
			for end < len(rs) && rs[end] != '}' {
				end++
			}
			if end < len(rs) && rs[end] == '}' {
				varName := string(rs[j+1 : end])
				return lookup(varName), end + 1
			}
		}
		for j < len(rs) && (rs[j] == '_' || rs[j] >= '0' && rs[j] <= '9' || rs[j] >= 'a' && rs[j] <= 'z' || rs[j] >= 'A' && rs[j] <= 'Z') {
			j++
		}
		if j == i+1 {
			return "$", j
		}
		return lookup(string(rs[i+1 : j])), j
	}

	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t':
			_ = flush()
		case c == '#' && !inWord:
			i = len(rs)
		case c == '|':
			_ = flush()
			if pending != 0 || (len(cur.args) == 0) {
				return nil, errors.New("syntax error near '|'")
			}
			cur.extractAssignments()
			stages = append(stages, cur)
			cur = stage{}
		case c == '<':
			_ = flush()
			pending = 1
		case c == '>':
			_ = flush()
			pending = 2
			if i+1 < len(rs) && rs[i+1] == '>' {
				pending = 3
				i++
			}
		case c == '\'':
			inWord, wLit = true, true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				word = append(word, rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated '")
			}
			i = j
		case c == '"':
			inWord, wLit = true, true
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				switch {
				case rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune("\"\\$", rs[j+1]):
					word = append(word, rs[j+1])
					j += 2
				case rs[j] == '$':
					v, nj := expand(rs, j)
					word = append(word, []rune(v)...)
					j = nj
				default:
					word = append(word, rs[j])
					j++
				}
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated \"")
			}
			i = j
		case c == '\\' && i+1 < len(rs):
			inWord, wLit = true, true
			i++
			word = append(word, rs[i])
		case c == '$':
			inWord = true
			v, nj := expand(rs, i)
			word = append(word, []rune(v)...)
			i = nj - 1
		default:
			inWord = true
			if c == '*' || c == '?' || c == '[' {
				wGlob = true
			}
			word = append(word, c)
		}
	}
	_ = flush()
	if pending != 0 {
		return nil, errors.New("syntax error: missing redirection target")
	}
	cur.extractAssignments()
	if len(cur.args) == 0 && len(cur.env) > 0 && len(stages) == 0 && cur.in == "" && cur.out == "" {
		return []stage{cur}, nil // assignment-only line: sets shell environment
	}
	if len(cur.args) == 0 {
		if len(stages) > 0 {
			return nil, errors.New("syntax error: empty command after '|'")
		}
		if cur.in != "" || cur.out != "" {
			return nil, errors.New("syntax error: redirection without command")
		}
		return nil, nil
	}
	return append(stages, cur), nil
}

func isStd(f *os.File) bool { return f == os.Stdin || f == os.Stdout || f == os.Stderr }

func (s *Shell) isStd(f *os.File) bool {
	return f == os.Stdin || f == os.Stdout || f == os.Stderr || f == s.in || f == s.out || f == s.err
}

// execute runs a parsed pipeline and returns the last stage's exit status.
func (s *Shell) execute(stages []stage) int {
	n := len(stages)
	ins := make([]*os.File, n)
	outs := make([]*os.File, n)
	var opened []*os.File // everything we must close on early failure
	fail := func(err error) int {
		for _, f := range opened {
			f.Close()
		}
		fmt.Fprintf(s.errFile(), "myos: %v\n", err)
		return 1
	}

	defaultIn := s.inFile()
	defaultOut := s.outFile()

	for i := range stages {
		if ins[i] == nil {
			ins[i] = defaultIn
		}
		outs[i] = defaultOut
		if i < n-1 {
			r, w, err := os.Pipe()
			if err != nil {
				return fail(err)
			}
			opened = append(opened, r, w)
			outs[i], ins[i+1] = w, r
		}
		if stages[i].in != "" {
			f, err := os.Open(stages[i].in)
			if err != nil {
				return fail(err)
			}
			opened = append(opened, f)
			if i > 0 {
				ins[i].Close() // unused pipe read end
			}
			ins[i] = f
		}
		if stages[i].out != "" {
			fl := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			if stages[i].appendOut {
				fl = os.O_WRONLY | os.O_CREATE | os.O_APPEND
			}
			f, err := os.OpenFile(stages[i].out, fl, 0o644)
			if err != nil {
				return fail(err)
			}
			opened = append(opened, f)
			if i < n-1 {
				outs[i].Close() // unused pipe write end; next stage gets EOF at once
			}
			outs[i] = f
		}
	}

	codes := make(chan [2]int, n)
	for i := range stages {
		go func(i int) {
			code := s.runStage(stages[i], ins[i], outs[i])
			// Close our ends so downstream readers see EOF.
			if !s.isStd(ins[i]) {
				ins[i].Close()
			}
			if !s.isStd(outs[i]) {
				outs[i].Close()
			}
			codes <- [2]int{i, code}
		}(i)
	}
	last := 0
	for range stages {
		c := <-codes
		if c[0] == n-1 {
			last = c[1]
		}
	}
	// Close any pipe/file ends that were created but unused by their stage
	// (e.g. a pipe whose writer stage redirected elsewhere).
	for _, f := range opened {
		f.Close()
	}
	return last
}
