package claudecode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session is one Claude Code session: its main conversation and the subagents
// it launched.
type Session struct {
	ID        string
	Path      string // the main transcript
	Main      *Thread
	Subagents []*Thread
}

// Turn is the work done to answer one message the user typed: a range of the
// main thread's requests plus the subagents launched from them.
type Turn struct {
	Number    int // 1-based
	Prompt    string
	At        time.Time
	From, To  int // main-thread request range [From, To)
	Subagents []*Thread
}

// LoadSession reads a session's main transcript and its subagent transcripts.
func LoadSession(projectsDir, mainPath string) (*Session, error) {
	dir, err := os.OpenRoot(projectsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }() // Read-only resource; read errors are checked below.
	rel, err := filepath.Rel(projectsDir, mainPath)
	if err != nil {
		return nil, err
	}
	rel = filepath.ToSlash(rel)
	main, err := parseThread(dir.FS(), rel)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mainPath, err)
	}
	s := &Session{ID: strings.TrimSuffix(filepath.Base(mainPath), ".jsonl"), Path: mainPath, Main: main}

	subPaths, err := fs.Glob(dir.FS(), strings.TrimSuffix(rel, ".jsonl")+"/subagents/agent-*.jsonl")
	if err != nil {
		return nil, err
	}
	sort.Strings(subPaths)
	for _, p := range subPaths {
		th, err := parseThread(dir.FS(), p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		s.Subagents = append(s.Subagents, th)
	}
	return s, nil
}

// FindSessions returns the main transcript paths under root (normally
// ~/.claude/projects) of sessions with at least one request made inside
// projectDir and modified since the given time, newest first.
func FindSessions(root, projectDir string, since time.Time) ([]string, error) {
	inProject, err := ProjectMatcher(projectDir)
	if err != nil {
		return nil, err
	}
	type found struct {
		path string
		mod  time.Time
	}
	var sessions []found
	dir, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }() // Read-only resource; read errors are checked below.
	err = fs.WalkDir(dir.FS(), ".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		th, err := parseThread(dir.FS(), path)
		if err != nil {
			return nil
		}
		for _, r := range th.Requests {
			if inProject(r.Cwd) {
				sessions = append(sessions, found{filepath.Join(root, filepath.FromSlash(path)), info.ModTime()})
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].mod.After(sessions[j].mod) })
	paths := make([]string, len(sessions))
	for i, s := range sessions {
		paths[i] = s.path
	}
	return paths, nil
}

// ProjectMatcher returns a predicate reporting whether a working directory is
// inside dir. Symlinks are resolved (macOS /tmp is /private/tmp).
func ProjectMatcher(dir string) (func(string) bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	roots := []string{abs}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		roots = append(roots, real)
	}
	return func(cwd string) bool {
		if cwd == "" {
			return false
		}
		for _, r := range roots {
			if cwd == r || strings.HasPrefix(cwd, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}, nil
}

// Turns groups the main thread's requests by the user message they answer and
// attaches each subagent to the turn whose Agent tool call launched it.
// Requests before the first typed message form a turn of their own.
func (s *Session) Turns() []Turn {
	prompts := s.Main.Prompts
	if len(prompts) == 0 || prompts[0].Gap > 0 {
		prompts = append([]Prompt{{Text: "(before first message)"}}, prompts...)
	}
	var turns []Turn
	for i, p := range prompts {
		to := len(s.Main.Requests)
		if i+1 < len(prompts) {
			to = prompts[i+1].Gap
		}
		if to <= p.Gap && i+1 < len(prompts) {
			continue // a message with no request of its own (e.g. queued)
		}
		turns = append(turns, Turn{Number: len(turns) + 1, Prompt: p.Text, At: p.At, From: p.Gap, To: to})
	}
	for _, sub := range s.Subagents {
		idx := len(turns) - 1
		if req, ok := s.Main.ToolCaller[sub.ParentToolUseID]; ok {
			for i, t := range turns {
				if req >= t.From && req < t.To {
					idx = i
				}
			}
		}
		if idx >= 0 {
			turns[idx].Subagents = append(turns[idx].Subagents, sub)
		}
	}
	return turns
}
