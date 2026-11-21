package agentusage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type codexReader struct{}

func (codexReader) defaultPath() string {
	home, _ := os.UserHomeDir()
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "sessions")
}

func (codexReader) index(ctx context.Context, s *Store, path string) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
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
}

func (codexReader) load(ctx context.Context, s *Store, e Entry) (*Session, error) {
	return codexSession(e)
}

func (codexReader) probe(ctx context.Context, model, home, work string, servers map[string]map[string]any) (int, error) {
	for _, cfg := range servers {
		cfg["required"] = true
	}

	auth := filepath.Join(filepath.Dir(DefaultPath("codex")), "auth.json")
	if _, err := os.Stat(auth); err == nil {
		if err = os.Symlink(auth, filepath.Join(home, "auth.json")); err != nil {
			return 0, err
		}
	}
	data, err := toml.Marshal(map[string]any{"mcp_servers": servers})
	if err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(home, "config.toml"), data, 0600); err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, "codex", "exec", "--ephemeral", "--skip-git-repo-check", "--json", "-s", "read-only", "-m", model, probePrompt)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	cmd.Dir = work
	data, err = cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("codex probe: %w", err)
	}
	input, err := measuredInput(data)
	if err != nil || input > 0 {
		return input, err
	}

	return 0, errors.New("client did not return a measured context count")
}
