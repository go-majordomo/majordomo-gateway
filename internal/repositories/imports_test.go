package repositories

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

func TestImportAtomicityAndTelemetry(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the PostgreSQL import test")
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	db.SetMaxOpenConns(1)
	// Use a session-local schema even when the supplied database already hosts a gateway.
	if _, err = db.Exec("CREATE TEMP TABLE import_test_init(id int); SET search_path TO pg_temp"); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	now := time.Now().UTC()
	log := &models.RequestLog{ID: uuid.New(), RequestMethod: "IMPORT", RequestedAt: now, RespondedAt: now, StatusCode: 200}
	logs := []*models.RequestLog{log}
	span := &models.ToolSpan{ID: uuid.New(), Model: strings.Repeat("x", 101), CalledAt: now}
	spans := []*models.ToolSpan{span}
	repo := NewImportRepository(db, nil, nil)
	if _, _, err := repo.SaveImport(ctx, logs, spans); err == nil {
		t.Fatal("expected the span insert to fail")
	}
	var rows int
	if err := db.Get(&rows, "SELECT count(*) FROM llm_requests"); err != nil || rows != 0 {
		t.Fatalf("partial import: %d rows, %v", rows, err)
	}
	span.Model = "test"
	for _, want := range []int{1, 0} {
		requests, tools, err := repo.SaveImport(ctx, logs, spans)
		if err != nil || requests != want || tools != want {
			t.Fatalf("retry: %d requests, %d spans, %v", requests, tools, err)
		}
	}
	live := *log
	live.ID = uuid.New()
	live.RequestMethod = "POST"
	live.StatusCode = 500
	live.ResponseTimeMs = 100
	if _, err := InsertRequest(ctx, db, nil, &live, false); err != nil {
		t.Fatal(err)
	}
	filter := &UsageFilter{Start: now.Add(-time.Minute), End: now.Add(time.Minute)}
	usage := NewUsageRepository(db)
	errors, err := usage.GetErrorSummary(ctx, filter)
	if err != nil || errors.TotalRequests != 1 || errors.ErrorRequests != 1 {
		t.Fatalf("errors: %+v, %v", errors, err)
	}
	latency, err := usage.GetLatencyStats(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if latency.AvgLatencyMs != 100 {
		t.Fatalf("latency includes imported request: %+v", latency)
	}
	health, err := NewEndpointHealthRepository(db).GetEndpointHealth(ctx, filter.Start)
	if err != nil || len(health) != 1 || health[0].SampleCount != 1 || health[0].ErrorRate != 1 {
		t.Fatalf("routing health: %+v, %v", health, err)
	}
}
