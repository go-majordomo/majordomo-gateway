package repositories

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/spanid"
)

// runTreeFetchCap bounds the number of leaves loaded when assembling a single run's
// tree. A run larger than this is rendered truncated rather than risking an unbounded
// scan/allocation.
const runTreeFetchCap = 5000

// maxSpanDepth bounds the interior-node depth materialized per leaf, guarding against
// a pathologically deep span_path. Deeper paths are attached at this depth.
const maxSpanDepth = 64

// ListRuns returns one row per agent run (trace_id) with usage rolled up across every
// LLM call in the run, most recently ended first.
func (r *UsageRepository) ListRuns(ctx context.Context, filter *UsageFilter, limit, offset int) ([]*models.RunListItem, int, error) {
	countQuery := `
		SELECT COUNT(DISTINCT trace_id)
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2 AND trace_id IS NOT NULL
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)`

	var total int
	if err := r.db.GetContext(ctx, &total, countQuery, filter.Start, filter.End, filter.APIKeyID); err != nil {
		return nil, 0, fmt.Errorf("count runs: %w", err)
	}

	// The label is the earliest call's top-level step name, or its span name / model
	// when the run is flat. array_agg(... ORDER BY requested_at) picks that first row
	// without a correlated subquery.
	query := `
		SELECT
			trace_id,
			(array_agg(
				CASE WHEN COALESCE(span_path, '') <> ''
				     THEN split_part(span_path, '/', 1)
				     ELSE COALESCE(span_name, model) END
				ORDER BY requested_at ASC
			))[1] AS label,
			COUNT(*) AS request_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost,
			COALESCE(SUM(cached_cost), 0) AS cached_cost,
			COALESCE(SUM(cache_creation_cost), 0) AS cache_creation_cost,
			MIN(requested_at) AS started_at,
			MAX(responded_at) AS ended_at
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2 AND trace_id IS NOT NULL
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
		GROUP BY trace_id
		ORDER BY ended_at DESC
		LIMIT $4 OFFSET $5`

	var runs []*models.RunListItem
	if err := r.db.SelectContext(ctx, &runs, query, filter.Start, filter.End, filter.APIKeyID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list runs: %w", err)
	}
	return runs, total, nil
}

// runLeafRow is the flat projection of one LLM call used to assemble a run tree.
type runLeafRow struct {
	ID             uuid.UUID  `db:"id"`
	SpanID         *uuid.UUID `db:"span_id"`
	ParentSpanID   *uuid.UUID `db:"parent_span_id"`
	SpanPath       string     `db:"span_path"`
	SpanName       string     `db:"span_name"`
	Provider       string     `db:"provider"`
	Model          string     `db:"model"`
	RequestedAt    time.Time  `db:"requested_at"`
	RespondedAt    time.Time  `db:"responded_at"`
	ResponseTimeMs int64      `db:"response_time_ms"`
	StatusCode     int        `db:"status_code"`
	TotalCost      float64    `db:"total_cost"`
}

// GetRunTree fetches every LLM call in a run and assembles the waterfall: a synthetic
// root, one interior node per named tool/agent step (derived from span_path), and the
// LLM calls as leaves, with cost and call count rolled up the tree. Returns (nil, nil)
// when the run has no calls. Interior tool/agent nodes have no rows of their own in
// Tier 1, so they are synthesized here from the leaves' paths.
func (r *UsageRepository) GetRunTree(ctx context.Context, filter *UsageFilter, traceID string) (*models.RunTree, error) {
	query := `
		SELECT
			id, span_id, parent_span_id,
			COALESCE(span_path, '') AS span_path,
			COALESCE(span_name, model) AS span_name,
			provider, model, requested_at, responded_at,
			response_time_ms, status_code, total_cost
		FROM llm_requests
		WHERE trace_id = $1
		ORDER BY requested_at ASC
		LIMIT $2`

	var rows []runLeafRow
	if err := r.db.SelectContext(ctx, &rows, query, traceID, runTreeFetchCap+1); err != nil {
		return nil, fmt.Errorf("fetch run tree: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	truncated := false
	if len(rows) > runTreeFetchCap {
		rows = rows[:runTreeFetchCap]
		truncated = true
	}

	var tools []toolSpanRow
	toolQuery := `
		SELECT id, span_path, tool_name, model, called_at,
			result_tokens, reread_count, attributed_cost
		FROM run_tool_spans
		WHERE trace_id = $1
		ORDER BY called_at ASC
		LIMIT $2`
	if err := r.db.SelectContext(ctx, &tools, toolQuery, traceID, runTreeFetchCap+1); err != nil {
		return nil, fmt.Errorf("fetch run tool spans: %w", err)
	}
	if len(tools) > runTreeFetchCap {
		tools = tools[:runTreeFetchCap]
		truncated = true
	}
	// The tool total covers every span of the run, not only the ones drawn.
	var toolTotal float64
	if err := r.db.GetContext(ctx, &toolTotal,
		`SELECT COALESCE(SUM(attributed_cost), 0) FROM run_tool_spans WHERE trace_id = $1`, traceID); err != nil {
		return nil, fmt.Errorf("sum run tool spans: %w", err)
	}

	tree := assembleRunTree(traceID, rows, truncated)
	attachToolSpans(tree, tools)
	tree.ToolCost = toolTotal
	return tree, nil
}

// toolSpanRow is the projection of one tool call (Tier 2) attached to a run tree.
type toolSpanRow struct {
	ID             uuid.UUID `db:"id"`
	SpanPath       string    `db:"span_path"`
	ToolName       string    `db:"tool_name"`
	Model          string    `db:"model"`
	CalledAt       time.Time `db:"called_at"`
	ResultTokens   int       `db:"result_tokens"`
	RereadCount    int       `db:"reread_count"`
	AttributedCost float64   `db:"attributed_cost"`
}

// attachToolSpans adds each tool call as a "tool" node under the step it ran in.
// A tool's cost is the share of that step's LLM cost spent carrying its result,
// already counted in the tree's totals, so it is reported as AttributedCost and
// summed into RunTree.ToolCost but never rolled up again. A tool whose step has no
// node in the tree (its calls were truncated away) hangs under the root.
func attachToolSpans(tree *models.RunTree, tools []toolSpanRow) {
	if tree == nil || len(tools) == 0 {
		return
	}
	byPath := map[string]*models.RunNode{}
	var index func(n *models.RunNode)
	index = func(n *models.RunNode) {
		if n.Kind != models.RunNodeKindLLM {
			byPath[n.Path] = n
		}
		for _, c := range n.Children {
			index(c)
		}
	}
	index(tree.Root)

	for i := range tools {
		t := &tools[i]
		parent, ok := byPath[spanid.CanonicalPath(t.SpanPath)]
		if !ok {
			parent = tree.Root
		}
		parent.Children = append(parent.Children, &models.RunNode{
			SpanID:         t.ID,
			ParentSpanID:   &parent.SpanID,
			Name:           t.ToolName,
			Path:           parent.Path,
			Kind:           models.RunNodeKindTool,
			Model:          &t.Model,
			RequestedAt:    &t.CalledAt,
			ResultTokens:   &t.ResultTokens,
			RereadCount:    &t.RereadCount,
			AttributedCost: t.AttributedCost,
		})
		tree.ToolCost += t.AttributedCost
	}
}

// assembleRunTree builds the waterfall from a run's LLM-call rows, already ordered by
// requested_at ascending. Kept separate from the DB fetch so it can be unit-tested.
func assembleRunTree(traceID string, rows []runLeafRow, truncated bool) *models.RunTree {
	root := &models.RunNode{
		SpanID: spanid.InteriorSpanID(traceID, ""),
		Name:   runLabel(rows[0]),
		Path:   "",
		Kind:   models.RunNodeKindRun,
	}
	nodes := map[string]*models.RunNode{"": root}

	// ensurePath materializes every interior step node on the way to canonicalPath,
	// reusing already-created ancestors, and returns the deepest one.
	ensurePath := func(canonicalPath string) *models.RunNode {
		if n, ok := nodes[canonicalPath]; ok {
			return n
		}
		prefixes := spanid.AncestorPaths(canonicalPath)
		if len(prefixes) > maxSpanDepth {
			prefixes = prefixes[:maxSpanDepth]
		}
		parent := root
		for _, prefix := range prefixes {
			if existing, ok := nodes[prefix]; ok {
				parent = existing
				continue
			}
			segments := spanid.SplitPath(prefix)
			node := &models.RunNode{
				SpanID:       spanid.InteriorSpanID(traceID, prefix),
				ParentSpanID: &parent.SpanID,
				Name:         segments[len(segments)-1],
				Path:         prefix,
				Kind:         models.RunNodeKindStep,
			}
			parent.Children = append(parent.Children, node)
			nodes[prefix] = node
			parent = node
		}
		return parent
	}

	endedAt := rows[0].RespondedAt
	for i := range rows {
		row := &rows[i]
		if row.RespondedAt.After(endedAt) {
			endedAt = row.RespondedAt
		}

		parent := ensurePath(spanid.CanonicalPath(row.SpanPath))

		spanID := row.ID
		if row.SpanID != nil {
			spanID = *row.SpanID
		}
		leaf := &models.RunNode{
			SpanID:         spanID,
			ParentSpanID:   &parent.SpanID,
			Name:           row.SpanName,
			Path:           parent.Path,
			Kind:           models.RunNodeKindLLM,
			RequestID:      &row.ID,
			Provider:       &row.Provider,
			Model:          &row.Model,
			RequestedAt:    &row.RequestedAt,
			RespondedAt:    &row.RespondedAt,
			ResponseTimeMs: &row.ResponseTimeMs,
			StatusCode:     &row.StatusCode,
			SelfCost:       row.TotalCost,
			TotalCost:      row.TotalCost,
			RequestCount:   1,
		}
		parent.Children = append(parent.Children, leaf)
	}

	rollUpRunNode(root)

	return &models.RunTree{
		TraceID:      traceID,
		Root:         root,
		TotalCost:    root.TotalCost,
		RequestCount: root.RequestCount,
		StartedAt:    rows[0].RequestedAt,
		EndedAt:      endedAt,
		Truncated:    truncated,
	}
}

// runLabel derives a run/root label from a leaf: its top-level step name, or its span
// name (already defaulted to the model) when the run is flat.
func runLabel(row runLeafRow) string {
	if segments := spanid.SplitPath(row.SpanPath); len(segments) > 0 {
		return segments[0]
	}
	return row.SpanName
}

// rollUpRunNode sums descendant cost and LLM-call counts into each interior node,
// bottom-up. Leaves already carry their own totals.
func rollUpRunNode(n *models.RunNode) {
	for _, child := range n.Children {
		rollUpRunNode(child)
		n.TotalCost += child.TotalCost
		n.RequestCount += child.RequestCount
	}
}
