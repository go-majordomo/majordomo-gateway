package agentusage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
)

type Entry struct {
	ID, Path, Project, Parent string
	Modified                  time.Time
	Projects, Children        []string
}
type Store struct {
	reader  reader
	Source  string
	entries []Entry
	db      *sql.DB
	// Skipped lists stores that could not be read ("path: error"); the
	// readable ones are still indexed.
	Skipped []string
}

// reader owns the native client's storage format, paths and context probe.
type reader interface {
	defaultPath() string
	index(context.Context, *Store, string) error
	load(context.Context, *Store, Entry) (*Session, error)
	probe(context.Context, string, string, string, map[string]map[string]any) (int, error)
}

var readers = map[string]reader{"codex": codexReader{}, "cursor": cursorReader{}}

func DefaultPath(source string) string {
	if r := readers[source]; r != nil {
		return r.defaultPath()
	}
	return ""
}
func Open(ctx context.Context, source, path string) (*Store, error) {
	r := readers[source]
	if r == nil {
		return nil, fmt.Errorf("unknown client %q", source)
	}
	s := &Store{Source: source, reader: r}
	if err := r.index(ctx, s, path); err != nil {
		s.Close()
		return nil, err
	}
	sort.SliceStable(s.entries, func(i, j int) bool { return s.entries[i].Modified.After(s.entries[j].Modified) })
	return s, nil
}

func (s *Store) Close() {
	if s.db != nil {
		_ = s.db.Close() // The store opens read-only databases.
	}
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
		if len(s.Skipped) > 0 {
			return nil, fmt.Errorf("no %s session matches project %q or id %q (%d stores could not be read: %s)",
				s.Source, project, id, len(s.Skipped), strings.Join(s.Skipped, "; "))
		}
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
	session, err := s.reader.load(ctx, s, e)
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
