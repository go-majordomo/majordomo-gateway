package services

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
	"github.com/go-majordomo/majordomo-gateway/internal/spanid"
)

type fakeImportRepo struct {
	logs  []*models.RequestLog
	spans []*models.ToolSpan
}

func (f *fakeImportRepo) SaveImport(_ context.Context, logs []*models.RequestLog, spans []*models.ToolSpan) (int, int, error) {
	f.logs = append(f.logs, logs...)
	f.spans = append(f.spans, spans...)
	return len(logs), len(spans), nil
}

// fakeCalc prices "claude-haiku-5-5" at $1/M input, $0.10/M cached, $5/M output,
// with cache writes at 1.25x (5m) / 2x (1h) input, mirroring pricing.Service.
type fakeCalc struct{}

func (fakeCalc) Calculate(m *models.UsageMetrics) models.Cost {
	if m.Model != "claude-haiku-5-5" {
		return models.Cost{}
	}
	fresh := float64(m.InputTokens-m.CachedTokens-m.CacheCreationTokens) / 1e6
	cached := float64(m.CachedTokens) * 0.1 / 1e6
	create := (float64(m.CacheCreation5mTokens)*1.25 + float64(m.CacheCreation1hTokens)*2) / 1e6
	out := float64(m.OutputTokens) * 5 / 1e6
	return models.Cost{InputCost: fresh, CachedCost: cached, CacheCreationCost: create, OutputCost: out,
		TotalCost: fresh + cached + create + out, ModelAliasFound: true}
}

func validImport() *models.ClaudeCodeImport {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	return &models.ClaudeCodeImport{
		SessionID: "sess-1",
		Metadata:  map[string]string{"team": "acme"},
		Requests: []models.ClaudeCodeImportRequest{
			{RequestID: "req_1", SpanPath: "claude-code/turn-1", Model: "claude-haiku-5-5", OccurredAt: at,
				InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000,
				CacheCreation5mTokens: 1_000_000, CacheCreation1hTokens: 1_000_000},
			{RequestID: "req_2", SpanPath: "/claude-code//turn-1/agent:x-1/", Model: "mystery", OccurredAt: at},
		},
		ToolSpans: []models.ClaudeCodeImportToolSpan{
			{ToolUseID: "toolu_1", SpanPath: "claude-code/turn-1", ToolName: "mcp__crm__get_leads",
				Model: "claude-haiku-5-5", CalledAt: at, ResultTokens: 1_000_000, Charges: []models.ModelInput{{Model: "claude-haiku-5-5", Prompt: 1000000, Input: 1000000, Write1h: 1000000}, {Model: "claude-haiku-5-5", Prompt: 1000000, Input: 1000000, Cached: 1000000}, {Model: "claude-haiku-5-5", Prompt: 1000000, Input: 1000000, Cached: 1000000}, {Model: "claude-haiku-5-5", Prompt: 1000000, Input: 1000000, Cached: 1000000}}},
		},
	}
}

func TestImportClaudeCode(t *testing.T) {
	repo := &fakeImportRepo{}
	svc := NewImportService(repo, fakeCalc{})
	key := uuid.New()

	res, err := svc.ImportClaudeCode(context.Background(), key, validImport())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.TraceID != "claude-code:"+key.String()+":sess-1" || res.Requests != 2 || res.Unpriced != 1 {
		t.Errorf("result = %+v", res)
	}
	// $1 input + $0.10 cached + $1.25 5m write + $2 1h write + $5 output
	if math.Abs(res.TotalCost-9.35) > 1e-9 {
		t.Errorf("total cost = %v, want 9.35", res.TotalCost)
	}
	// one 1h write ($2) + three cached re-reads ($0.30)
	if math.Abs(res.ToolCost-2.30) > 1e-9 {
		t.Errorf("tool cost = %v, want 2.30", res.ToolCost)
	}

	log := repo.logs[0]
	if log.InputTokens != 4_000_000 || log.CachedTokens != 1_000_000 || log.CacheCreationTokens != 2_000_000 {
		t.Errorf("log tokens = in %d cached %d create %d", log.InputTokens, log.CachedTokens, log.CacheCreationTokens)
	}
	if *log.MajordomoAPIKeyID != key || log.RawMetadata["source"] != "claude-code" || log.RawMetadata["team"] != "acme" {
		t.Errorf("log attribution = key %v metadata %v", *log.MajordomoAPIKeyID, log.RawMetadata)
	}
	if *log.SpanID != log.ID || *log.ParentSpanID != spanid.InteriorSpanID("claude-code:"+key.String()+":sess-1", "claude-code/turn-1") {
		t.Errorf("span ids do not follow spanid conventions")
	}
	if p := *repo.logs[1].SpanPath; p != "claude-code/turn-1/agent:x-1" {
		t.Errorf("span path not canonicalized: %q", p)
	}
	if s := repo.spans[0]; s.MCPServer == nil || *s.MCPServer != "crm" || s.ParentSpanID != *log.ParentSpanID {
		t.Errorf("tool span = %+v", s)
	}

	// Re-importing derives the same ids, so the repository can skip them.
	repo2 := &fakeImportRepo{}
	if _, err := NewImportService(repo2, fakeCalc{}).ImportClaudeCode(context.Background(), key, validImport()); err != nil {
		t.Fatal(err)
	}
	if repo2.logs[0].ID != log.ID || repo2.spans[0].ID != repo.spans[0].ID {
		t.Error("ids are not deterministic")
	}
}

func TestImportClaudeCodeValidation(t *testing.T) {
	cases := map[string]func(*models.ClaudeCodeImport){
		"session":     func(i *models.ClaudeCodeImport) { i.SessionID = "" },
		"no requests": func(i *models.ClaudeCodeImport) { i.Requests = nil },
		"request id":  func(i *models.ClaudeCodeImport) { i.Requests[0].RequestID = "" },
		"model":       func(i *models.ClaudeCodeImport) { i.Requests[0].Model = "" },
		"time":        func(i *models.ClaudeCodeImport) { i.Requests[0].OccurredAt = time.Time{} },
		"negative":    func(i *models.ClaudeCodeImport) { i.Requests[0].OutputTokens = -1 },
		"tool name":   func(i *models.ClaudeCodeImport) { i.ToolSpans[0].ToolName = "" },
		"1h > total":  func(i *models.ClaudeCodeImport) { i.ToolSpans[0].Charges[0].Write1h = 2_000_000 },
		"metadata":    func(i *models.ClaudeCodeImport) { i.Metadata[""] = "x" },
	}
	for name, mutate := range cases {
		imp := validImport()
		mutate(imp)
		_, err := NewImportService(&fakeImportRepo{}, fakeCalc{}).ImportClaudeCode(context.Background(), uuid.New(), imp)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: expected ErrValidation, got %v", name, err)
		}
	}
}

// tierCalc is fakeCalc at 5x for a prompt above 100K tokens.
type tierCalc struct{}

func (tierCalc) Calculate(m *models.UsageMetrics) models.Cost {
	c := fakeCalc{}.Calculate(m)
	if max(m.InputTokens, m.PromptTokens) > 100_000 {
		c.TotalCost *= 5
	}
	return c
}

// Each part of a tool result's cost is priced at the tier of the request it
// was carried in: the write and one re-read here sit in long prompts.
func TestImportToolSpanPricedPerRequestTier(t *testing.T) {
	imp := validImport()
	imp.ToolSpans[0].ResultTokens = 1000
	imp.ToolSpans[0].Charges = []models.ModelInput{
		{Model: "claude-haiku-5-5", Prompt: 150000, Input: 1000, Write1h: 1000},
		{Model: "claude-haiku-5-5", Prompt: 150000, Input: 1000, Cached: 1000},
		{Model: "claude-haiku-5-5", Prompt: 50000, Input: 1000, Cached: 1000},
	}

	res, err := NewImportService(&fakeImportRepo{}, tierCalc{}).ImportClaudeCode(context.Background(), uuid.New(), imp)
	if err != nil {
		t.Fatal(err)
	}
	// write 1000 x $2/M x 5, re-reads 1000 x $0.10/M x 5 and x 1
	want := (1000*2*5 + 1000*0.1*5 + 1000*0.1) / 1e6
	if math.Abs(res.ToolCost-want) > 1e-12 {
		t.Errorf("tool cost = %v, want %v", res.ToolCost, want)
	}

	imp.ToolSpans[0].Charges = nil
	if _, err := NewImportService(&fakeImportRepo{}, tierCalc{}).ImportClaudeCode(context.Background(), uuid.New(), imp); !errors.Is(err, ErrValidation) {
		t.Errorf("missing charges: err = %v", err)
	}
}

// The same session imported under two API keys gets separate rows: ids are
// derived from the key, so one key's import can never update the other's.
func TestImportIdsAreScopedToTheKey(t *testing.T) {
	keyA, keyB := uuid.New(), uuid.New()
	repoA, repoA2, repoB := &fakeImportRepo{}, &fakeImportRepo{}, &fakeImportRepo{}
	for _, c := range []struct {
		repo *fakeImportRepo
		key  uuid.UUID
	}{{repoA, keyA}, {repoA2, keyA}, {repoB, keyB}} {
		if _, err := NewImportService(c.repo, fakeCalc{}).ImportClaudeCode(context.Background(), c.key, validImport()); err != nil {
			t.Fatal(err)
		}
	}
	if repoA.logs[0].ID != repoA2.logs[0].ID || repoA.spans[0].ID != repoA2.spans[0].ID {
		t.Error("re-importing under the same key must derive the same ids")
	}
	if *repoA.logs[0].TraceID == *repoB.logs[0].TraceID || repoA.logs[0].ID == repoB.logs[0].ID || repoA.spans[0].ID == repoB.spans[0].ID {
		t.Error("another key must derive different ids")
	}
}

// A result carried by a request that wrote nothing to the cache was sent as
// ordinary input and is priced as such, not as a 5-minute cache write.
func TestImportUncachedToolResult(t *testing.T) {
	imp := validImport()
	imp.ToolSpans[0].ResultTokens = 1000
	imp.ToolSpans[0].Charges = []models.ModelInput{{Model: "claude-haiku-5-5", Prompt: 1000, Input: 1000}}
	res, err := NewImportService(&fakeImportRepo{}, fakeCalc{}).ImportClaudeCode(context.Background(), uuid.New(), imp)
	if err != nil {
		t.Fatal(err)
	}
	if want := 1000 * 1.0 / 1e6; math.Abs(res.ToolCost-want) > 1e-12 {
		t.Errorf("tool cost = %v, want %v (input rate, not 1.25x)", res.ToolCost, want)
	}

	imp.ToolSpans[0].Charges[0].Write1h = 1001
	if _, err := NewImportService(&fakeImportRepo{}, fakeCalc{}).ImportClaudeCode(context.Background(), uuid.New(), imp); !errors.Is(err, ErrValidation) {
		t.Errorf("uncached + 1h above result tokens: err = %v", err)
	}
}

func TestImportOmittedPromptUsesBaseTier(t *testing.T) {
	imp := validImport()
	span := &imp.ToolSpans[0]
	span.ResultTokens = 150000
	span.Charges = []models.ModelInput{{Model: span.Model, Input: 150000}}
	calc := pricing.NewService("", "../../model_catalog.json", "../../model_aliases.json", time.Hour)
	defer calc.Close()
	got, err := NewImportService(&fakeImportRepo{}, calc).ImportClaudeCode(context.Background(), uuid.New(), imp)
	if err != nil || math.Abs(got.ToolCost-.015) > 1e-12 {
		t.Fatalf("base-tier import: %+v, %v", got, err)
	}
}

func TestImportRereadsCountCarriedTokens(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", "unpriced-model"} {
		t.Run(model, func(t *testing.T) {
			imp := validImport()
			span := &imp.ToolSpans[0]
			span.Model = model
			span.ResultTokens = 100
			span.Charges = []models.ModelInput{
				{Model: model, Input: 100},
				{Model: model, Input: 50},
				{Model: model},
				{Model: model},
			}
			repo := &fakeImportRepo{}
			if _, err := NewImportService(repo, fakeCalc{}).ImportClaudeCode(context.Background(), uuid.New(), imp); err != nil {
				t.Fatal(err)
			}
			if got := repo.spans[0].RereadCount; got != 1 {
				t.Fatalf("rereads = %d, want 1 request carrying tokens", got)
			}
		})
	}
}
