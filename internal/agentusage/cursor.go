package agentusage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// Cursor desktop stores one composer header and a separately keyed row per
// bubble. Header order is the conversation order, even when timestamps tie.
func (s *Store) indexCursor(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT substr(key,14), coalesce(json_extract(value,'$.lastUpdatedAt'),json_extract(value,'$.createdAt'),0),
 coalesce(json_extract(value,'$.trackedGitRepos'),'[]'),coalesce(json_extract(value,'$.subagentComposerIds'),'[]')
 FROM cursorDiskKV WHERE key GLOB 'composerData:*' AND json_array_length(json_extract(value,'$.fullConversationHeadersOnly'))>0`)
	if err != nil {
		return fmt.Errorf("read Cursor composer index: %w", err)
	}
	defer func() { _ = rows.Close() }() // Read-only resource; read errors are checked below.
	parents := map[string]string{}
	for rows.Next() {
		var e Entry
		var ms int64
		var repos, children string
		if err = rows.Scan(&e.ID, &ms, &repos, &children); err != nil {
			return err
		}
		var paths []struct{ RepoPath string }
		if err = json.Unmarshal([]byte(repos), &paths); err != nil {
			return err
		}
		for _, r := range paths {
			e.Projects = append(e.Projects, r.RepoPath)
		}
		e.Modified = time.UnixMilli(ms)
		if err = json.Unmarshal([]byte(children), &e.Children); err != nil {
			return err
		}
		for _, child := range e.Children {
			parents[child] = e.ID
		}
		s.entries = append(s.entries, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// The IDE also indexes composers by workspace, including non-Git projects.
	var storePath string
	if err = s.db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&storePath); err != nil {
		return err
	}
	workspaceDirs, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(storePath)), "workspaceStorage", "*", "workspace.json"))
	workspaceByID := map[string]string{}
	for _, p := range workspaceDirs {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var w struct{ Folder string }
		if json.Unmarshal(data, &w) != nil {
			continue
		}
		u, err := url.Parse(w.Folder)
		if err != nil || u.Scheme != "file" {
			continue
		}
		// The folder's transcript index provides composer ids without reading chats.
		home, _ := os.UserHomeDir()
		slug := strings.Trim(strings.NewReplacer("/", "-", "\\", "-", ":", "").Replace(u.Path), "-")
		files, _ := os.ReadDir(filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts"))
		for _, f := range files {
			workspaceByID[strings.TrimSuffix(f.Name(), ".jsonl")] = u.Path
		}
	}
	for i := range s.entries {
		e := &s.entries[i]
		e.Parent = parents[e.ID]
		e.Project = workspaceByID[e.ID]
	}
	return nil
}

type bubble struct {
	Type           int
	BubbleID       string
	Text           string
	CreatedAt      time.Time
	ModelInfo      struct{ ModelName string }
	TokenCount     struct{ InputTokens, OutputTokens, CachedInputTokens int }
	ToolFormerData *struct{ ToolCallID, Name, RawArgs, Result string }
}

func (s *Store) cursorSession(ctx context.Context, e Entry) (*Session, error) {
	var hdr struct {
		CreatedAt   int64
		ModelConfig struct {
			ModelName      string
			SelectedModels []struct{ ModelID string }
		}
		PromptTokenBreakdown struct {
			TotalUsedTokens int
			Categories      []struct {
				ID              string
				EstimatedTokens int
			}
		}
		GeneratingBubbleIDs []string
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Read-only transaction; no writes to commit.
	var raw []byte
	err = tx.QueryRowContext(ctx, "SELECT value FROM cursorDiskKV WHERE key=?", "composerData:"+e.ID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &hdr); err != nil {
		return nil, err
	}
	session := &Session{ID: e.ID, Source: "cursor", Started: time.UnixMilli(hdr.CreatedAt)}
	rows, err := tx.QueryContext(ctx, `SELECT b.value FROM cursorDiskKV c JOIN json_each(c.value,'$.fullConversationHeadersOnly') h
 JOIN cursorDiskKV b ON b.key='bubbleId:'||?||':'||json_extract(h.value,'$.bubbleId')
 WHERE c.key=? ORDER BY CAST(h.key AS INTEGER)`, e.ID, "composerData:"+e.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // Read-only resource; read errors are checked below.
	generating := map[string]bool{}
	for _, id := range hdr.GeneratingBubbleIDs {
		generating[id] = true
	}
	var pending []Result
	history := 0
	model := hdr.ModelConfig.ModelName
	if len(hdr.ModelConfig.SelectedModels) == 1 {
		model = hdr.ModelConfig.SelectedModels[0].ModelID
	}
	if model == "" {
		model = "unknown"
	}
	turn, prompt := "", ""
	for rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var b bubble
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		if generating[b.BubbleID] {
			continue
		}
		if b.Type == 1 {
			turn = b.BubbleID
			prompt = b.Text
			history += tokens(len(b.Text))
			if b.ModelInfo.ModelName != "" {
				model = b.ModelInfo.ModelName
			}
			continue
		}
		if b.Type != 2 {
			continue
		}
		t := b.ToolFormerData
		u := b.TokenCount
		if t == nil && b.Text == "" && u.InputTokens+u.OutputTokens == 0 {
			continue
		}
		input, output := u.InputTokens, u.OutputTokens
		if input == 0 && output == 0 {
			session.Estimated = true
			input = history
			size := len(b.Text)
			if t != nil {
				size += len(t.Name) + len(t.RawArgs)
			}
			output = tokens(size)
		}
		if u.CachedInputTokens < 0 || input < u.CachedInputTokens || output < 0 {
			return nil, errors.New("invalid Cursor token counters")
		}
		at := b.CreatedAt
		if at.IsZero() {
			at = session.Started
			session.EstimatedTime = true
		}
		session.Requests = append(session.Requests, Request{ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{RequestID: b.BubbleID, OccurredAt: at, Model: model, InputTokens: input - u.CachedInputTokens, OutputTokens: output, CacheReadTokens: u.CachedInputTokens}, Turn: turn, Prompt: prompt, Results: pending})
		pending = nil
		history = input + output
		if t != nil && t.Result != "" {
			name := t.Name
			var args struct{ ProviderIdentifier, ToolName string }
			if json.Unmarshal([]byte(t.RawArgs), &args) == nil && args.ProviderIdentifier != "" && args.ToolName != "" {
				name = "mcp__" + args.ProviderIdentifier + "__" + args.ToolName
			}
			if name == "" {
				name = "unknown"
			}
			id := t.ToolCallID
			if id == "" {
				id = b.BubbleID
			}
			n := tokens(len(t.Result))
			pending = []Result{{id, name, n}}
			history += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Cursor's breakdown is one context snapshot. Applying it to every
	// historical request would invent measurements for earlier configurations.
	if len(session.Requests) > 0 && hdr.PromptTokenBreakdown.TotalUsedTokens > 0 {
		last := &session.Requests[len(session.Requests)-1]
		for _, c := range hdr.PromptTokenBreakdown.Categories {
			if c.ID == "mcp" && c.EstimatedTokens > 0 {
				last.Definitions = map[string]int{"(Cursor MCP snapshot)": c.EstimatedTokens}
			}
		}
	}
	return session, nil
}
