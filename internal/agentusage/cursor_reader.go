package agentusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type cursorReader struct{}

func (cursorReader) defaultPath() string {
	config, _ := os.UserConfigDir()
	if runtime.GOOS == "windows" && os.Getenv("APPDATA") != "" {
		config = os.Getenv("APPDATA")
	}
	return filepath.Join(config, "Cursor", "User", "globalStorage", "state.vscdb")
}

func (cursorReader) index(ctx context.Context, s *Store, path string) error {
	info, statErr := os.Stat(path)
	cliDir := ""
	if statErr == nil && info.IsDir() {
		cliDir = path
	} else {
		if statErr != nil && (!errors.Is(statErr, os.ErrNotExist) || path != (cursorReader{}).defaultPath()) {
			return statErr
		}
		if statErr == nil {
			db, err := cursorDB(path)
			if err != nil {
				return err
			}
			s.db = db
			if err = s.indexCursor(ctx); err != nil {
				return err
			}
		}
		if path == (cursorReader{}).defaultPath() {
			dataDir := os.Getenv("CURSOR_DATA_DIR")
			if dataDir == "" {
				home, _ := os.UserHomeDir()
				dataDir = filepath.Join(home, ".cursor")
			}
			cliDir = filepath.Join(dataDir, "chats")
		}
	}
	if cliDir != "" {
		if err := s.indexCursorCLI(ctx, cliDir); err != nil {
			return err
		}
	}

	return nil
}

func (cursorReader) load(ctx context.Context, s *Store, e Entry) (*Session, error) {
	if e.Path != "" {
		return cursorCLISession(ctx, e)
	}
	return s.cursorSession(ctx, e)
}

func (cursorReader) probe(ctx context.Context, model, home, work string, servers map[string]map[string]any) (int, error) {

	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(home, "mcp.json"), data, 0600); err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, "cursor-agent", "--print", "--mode", "ask", "--approve-mcps", "--output-format", "json", "--model", model, probePrompt)
	cmd.Env = append(os.Environ(), "CURSOR_CONFIG_DIR="+home, "CURSOR_DATA_DIR="+home)
	cmd.Dir = work
	data, err = cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && bytes.Contains(exit.Stderr, []byte("Authentication required")) {
			return 0, errors.New("cursor CLI requires authentication; sign in or set CURSOR_API_KEY for a live probe")
		}

		return 0, fmt.Errorf("cursor probe: %w", err)
	}
	input, err := measuredInput(data)
	if err != nil || input > 0 {
		return input, err
	}

	files, _ := filepath.Glob(filepath.Join(home, "chats", "*", "*", "store.db"))
	if len(files) == 1 {
		db, err := cursorDB(files[0])
		if err != nil {
			return 0, err
		}
		defer func() { _ = db.Close() }() // Read-only resource; read errors are checked below.
		_, root, err := cursorRoot(ctx, db)
		if err != nil {
			return 0, err
		}
		if root.ContextTokens > 0 {
			return root.ContextTokens, nil
		}
	}

	return 0, errors.New("client did not return a measured context count")
}
