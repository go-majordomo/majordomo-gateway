package agentusage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
)

type Entry struct {
	ID, Path, Project, Parent string
	Modified                  time.Time
	Projects, Children        []string
}
type Store struct {
	Source  string
	entries []Entry
}

func DefaultPath(source string) string {
	home, _ := os.UserHomeDir()
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "sessions")
}

func Open(ctx context.Context, source, path string) (*Store, error) {
	s := &Store{Source: source}
	if source == "codex" {
		err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".jsonl") && !strings.HasSuffix(p, ".jsonl.zst") {
				return nil
			}
			var meta struct {
				Type    string
				Payload struct {
					ID, Cwd string
					Source  json.RawMessage
				}
			}
			err = readLines(p, func(line []byte) error {
				if err := json.Unmarshal(line, &meta); err != nil {
					return err
				}
				return io.EOF
			})
			if err != nil {
				return fmt.Errorf("read rollout header %s: %w", p, err)
			}
			if meta.Type != "session_meta" || meta.Payload.ID == "" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			var src struct {
				Subagent struct {
					ThreadSpawn struct {
						Parent string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				}
			}
			_ = json.Unmarshal(meta.Payload.Source, &src)
			s.entries = append(s.entries, Entry{ID: meta.Payload.ID, Project: meta.Payload.Cwd, Parent: src.Subagent.ThreadSpawn.Parent, Path: p, Modified: info.ModTime()})
			return ctx.Err()
		})
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("unknown client %q", source)
	}
	sort.SliceStable(s.entries, func(i, j int) bool { return s.entries[i].Modified.After(s.entries[j].Modified) })
	return s, nil
}

func (s *Store) Find(project, id string, since time.Time) ([]Entry, error) {
	matches, err := claudecode.ProjectMatcher(project)
	if err != nil {
		return nil, err
	}
	var found []Entry
	for _, e := range s.entries {
		if id != "" {
			if e.ID == id {
				return []Entry{e}, nil
			}
			if strings.HasPrefix(e.ID, id) {
				found = append(found, e)
			}
			continue
		}
		match := matches(e.Project)
		for _, p := range e.Projects {
			match = match || matches(p)
		}
		if e.Parent == "" && match && !e.Modified.Before(since) {
			found = append(found, e)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no %s session matches project %q or id %q", s.Source, project, id)
	}
	if id != "" && len(found) > 1 {
		return nil, fmt.Errorf("ambiguous %s session prefix %q", s.Source, id)
	}
	return found, nil
}

func (s *Store) Load(ctx context.Context, e Entry) (*Session, error) {
	return s.load(ctx, e, map[string]bool{})
}

func (s *Store) load(ctx context.Context, e Entry, visited map[string]bool) (*Session, error) {
	visited[e.ID] = true
	session, err := codexSession(e)

	if err != nil {
		return nil, err
	}
	for _, child := range s.entries {
		if child.Parent != e.ID || visited[child.ID] {
			continue
		}
		sub, err := s.load(ctx, child, visited)
		if err != nil {
			return nil, err
		}
		session.Children = append(session.Children, sub)
	}
	return session, nil
}

func readLines(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // Read-only resource; read errors are checked below.
	var r io.Reader = f
	if strings.HasSuffix(path, ".zst") {
		z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return err
		}
		defer z.Close()
		r = z
	}
	b := bufio.NewReader(r)
	for {
		line, err := b.ReadBytes('\n')
		if len(line) > 0 {
			if errors.Is(err, io.EOF) && !json.Valid(line) {
				return nil
			} // a live rollout can end mid-write
			if e := fn(line); e != nil {
				if errors.Is(e, io.EOF) {
					return nil
				}
				return e
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
