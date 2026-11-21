package agentusage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// Cursor CLI stores the current context as an ordered list of content-addressed
// messages in the root ConversationStateStructure. Iterating the blobs table
// itself would include obsolete versions, abandoned branches and duplicate text.
type cliMeta struct {
	AgentID, LatestRootBlobID, LastUsedModel string
	CreatedAt                                int64
}
type cliRoot struct {
	ContextTokens int
	Messages      []string
	Projects      []string
}

func cursorDB(path string) (*sql.DB, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(p), RawQuery: "mode=ro"}
	// Archived WAL-mode stores can be copied without the two sidecar files.
	// Open those read-only snapshots without creating sidecars in the user's history.
	if _, err := os.Stat(p + "-wal"); errors.Is(err, os.ErrNotExist) {
		uri.RawQuery += "&immutable=1"
	}
	return sql.Open("sqlite", uri.String())
}

func cursorRoot(ctx context.Context, db *sql.DB) (cliMeta, cliRoot, error) {
	var meta cliMeta
	var root cliRoot
	var encoded string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='0'").Scan(&encoded); err != nil {
		return meta, root, err
	}
	data, err := hex.DecodeString(encoded)
	if err != nil {
		return meta, root, err
	}
	if err = json.Unmarshal(data, &meta); err != nil {
		return meta, root, err
	}
	if meta.LatestRootBlobID == "" {
		return meta, root, nil
	}
	if err = db.QueryRowContext(ctx, "SELECT data FROM blobs WHERE id=?", meta.LatestRootBlobID).Scan(&data); err != nil {
		return meta, root, err
	}
	for len(data) > 0 {
		field, t, n := protowire.ConsumeTag(data)
		if n < 0 {
			return meta, root, protowire.ParseError(n)
		}
		data = data[n:]
		n = protowire.ConsumeFieldValue(field, t, data)
		if n < 0 {
			return meta, root, protowire.ParseError(n)
		}
		if t == protowire.BytesType {
			b, _ := protowire.ConsumeBytes(data)
			switch field {
			case 1:
				root.Messages = append(root.Messages, hex.EncodeToString(b))
			case 5: // ConversationTokenDetails.used_tokens (field 1).
				for len(b) > 0 {
					field, typ, n := protowire.ConsumeTag(b)
					if n < 0 {
						return meta, root, protowire.ParseError(n)
					}
					b = b[n:]
					n = protowire.ConsumeFieldValue(field, typ, b)
					if n < 0 {
						return meta, root, protowire.ParseError(n)
					}
					if field == 1 && typ == protowire.VarintType {
						v, _ := protowire.ConsumeVarint(b)
						root.ContextTokens = int(v)
					}
					b = b[n:]
				}
			case 9:
				u, err := url.Parse(string(b))
				if err == nil && u.Scheme == "file" {
					root.Projects = append(root.Projects, u.Path)
				}
			}
		}
		data = data[n:]
	}
	return meta, root, nil
}

func (s *Store) indexCursorCLI(ctx context.Context, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*", "*", "store.db"))
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, e := range s.entries {
		ids[e.ID] = true
	}
	// A store that cannot be read is skipped and recorded, so it never hides
	// the healthy ones.
	skip := func(p string, err error) { s.Skipped = append(s.Skipped, p+": "+err.Error()) }
	for _, p := range files {
		db, err := cursorDB(p)
		if err != nil {
			skip(p, err)
			continue
		}
		m, root, err := cursorRoot(ctx, db)
		_ = db.Close() // Read-only store; report the cursorRoot error below.
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			skip(p, err)
			continue
		}
		if ids[m.AgentID] || len(root.Messages) == 0 || m.AgentID == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			skip(p, err)
			continue
		}
		mod := info.ModTime()
		if wal, err := os.Stat(p + "-wal"); err == nil && wal.ModTime().After(mod) {
			mod = wal.ModTime()
		}
		s.entries = append(s.entries, Entry{ID: m.AgentID, Path: p, Projects: root.Projects, Modified: mod})
		ids[m.AgentID] = true
	}
	return nil
}

func cursorCLISession(ctx context.Context, e Entry) (*Session, error) {
	db, err := cursorDB(e.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }() // Read-only resource; read errors are checked below.
	meta, root, err := cursorRoot(ctx, db)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: e.ID, Source: "cursor", Estimated: true, Started: time.UnixMilli(meta.CreatedAt)}
	model := meta.LastUsedModel
	if model == "" {
		model = "unknown"
	}
	turn, prompt := "", ""
	history := 0
	var pending []Result
	for _, id := range root.Messages {
		var data []byte
		if err := db.QueryRowContext(ctx, "SELECT data FROM blobs WHERE id=?", id).Scan(&data); err != nil {
			return nil, err
		}
		var msg struct {
			CreatedAt json.RawMessage
			Timestamp json.RawMessage
			Role      string
			Content   json.RawMessage
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, err
		}
		type block struct {
			Type, Text, ToolCallID, ToolName string
			Result                           json.RawMessage
		}
		var blocks []block
		size := len(msg.Content)
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			var text string
			if err = json.Unmarshal(msg.Content, &text); err != nil {
				return nil, err
			}
			size = len(text)
			blocks = []block{{Type: "text", Text: text}}
		}
		switch msg.Role {
		case "user":
			turn = id
			for _, b := range blocks {
				if b.Type == "text" {
					prompt = b.Text
					break
				}
			}
		case "tool":
			for _, b := range blocks {
				if b.Type == "tool-result" {
					name := b.ToolName
					if name == "" {
						name = "unknown"
					}
					callID := b.ToolCallID
					if callID == "" {
						callID = id
					}
					pending = append(pending, Result{callID, name, tokens(len(b.Result))})
				}
			}
		case "assistant":
			at := messageTime(msg.CreatedAt)
			if at.IsZero() {
				at = messageTime(msg.Timestamp)
			}
			if at.IsZero() {
				at = s.Started
				s.EstimatedTime = true
			}
			s.Requests = append(s.Requests, Request{ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{RequestID: id, OccurredAt: at, Model: model, InputTokens: history, OutputTokens: tokens(size)}, Turn: turn, Prompt: prompt, Results: pending})
			pending = nil
		}
		history += tokens(size)
	}
	return s, nil
}

// Cursor stores message times as RFC3339 strings or Unix milliseconds.
func messageTime(raw json.RawMessage) time.Time {
	var at time.Time
	if json.Unmarshal(raw, &at) == nil {
		return at
	}
	var ms int64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}
