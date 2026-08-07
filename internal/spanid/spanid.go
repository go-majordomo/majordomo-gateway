// Package spanid derives stable identities for the interior (tool/agent) nodes of
// an agent-run call tree.
//
// In Tier 1 of agent-run observability, only LLM calls produce rows in
// llm_requests; the tool/agent steps between them are described by the
// X-Majordomo-Span-Path header and never hit the proxy directly. Every layer that
// needs an id for such an interior node — request capture, dashboard read
// assembly, and (later) Tier 2 tool-span ingest — must derive the *same* id from
// the same (trace_id, path) inputs, or the tree won't reconcile. This package is
// the single source of truth for that derivation.
package spanid

import (
	"strings"

	"github.com/google/uuid"
)

// runSpanNamespace is a fixed, checked-in UUIDv5 namespace. It must never change:
// altering it would repoint every derived interior-node id and break joins against
// already-persisted rows.
var runSpanNamespace = uuid.MustParse("6f1a2b3c-0000-5000-a000-000000000001")

// pathSeparator is the reserved separator between step names in a span path.
// A literal separator inside a single step name must be percent-encoded by the
// client so it does not split into phantom nodes.
const pathSeparator = "/"

// InteriorSpanID returns the deterministic id of the interior node identified by
// canonicalPath within the given run. canonicalPath must already be canonical (see
// CanonicalPath); the empty string denotes the run's synthetic root node.
//
// The trace id and path are joined with a NUL byte so that no combination of
// trace-id and path characters can collide with a different pair.
func InteriorSpanID(traceID, canonicalPath string) uuid.UUID {
	return uuid.NewSHA1(runSpanNamespace, []byte(traceID+"\x00"+canonicalPath))
}

// SplitPath splits a span path into its ordered step names, dropping empty
// segments produced by leading, trailing, or repeated separators. A blank path
// yields nil.
func SplitPath(path string) []string {
	if path == "" {
		return nil
	}
	raw := strings.Split(path, pathSeparator)
	segments := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" {
			segments = append(segments, s)
		}
	}
	return segments
}

// CanonicalPath normalizes a span path to a stable form (empty segments removed,
// single separators) so the same logical path always derives the same interior
// id regardless of how the client formatted it. The empty/blank path canonicalizes
// to "".
func CanonicalPath(path string) string {
	return strings.Join(SplitPath(path), pathSeparator)
}

// AncestorPaths returns the canonical path prefixes of canonicalPath from the
// outermost step inward, e.g. "a/b/c" -> ["a", "a/b", "a/b/c"]. A blank path
// yields nil. Used by read assembly to materialize every interior node on the way
// down to a leaf.
func AncestorPaths(canonicalPath string) []string {
	segments := SplitPath(canonicalPath)
	if len(segments) == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(segments))
	for i := range segments {
		prefixes = append(prefixes, strings.Join(segments[:i+1], pathSeparator))
	}
	return prefixes
}
