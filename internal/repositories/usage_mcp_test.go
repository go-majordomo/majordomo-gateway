package repositories

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

func TestMCPServerUsagePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run PostgreSQL aggregation test")
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
	_, err = db.Exec(`
 CREATE TEMP TABLE llm_requests(trace_id text,majordomo_api_key_id uuid,request_method text,raw_metadata jsonb,indexed_metadata jsonb);
 CREATE TEMP TABLE run_tool_spans(trace_id text,majordomo_api_key_id uuid,mcp_server text,called_at timestamptz,
 provider text,model text,result_tokens integer,reread_count integer,attributed_cost numeric);
 INSERT INTO llm_requests VALUES
 ('shared','00000000-0000-0000-0000-000000000001','IMPORT','{"usage_basis":"estimated"}','{"team":"A"}'),
 ('shared','00000000-0000-0000-0000-000000000001','IMPORT','{"usage_basis":"estimated"}','{"team":"A"}'),
 ('shared','00000000-0000-0000-0000-000000000002','IMPORT','{}','{"team":"B"}');
 INSERT INTO run_tool_spans VALUES
 ('shared','00000000-0000-0000-0000-000000000001','maps',now(),'openai','test',100,2,0.01),
 ('shared','00000000-0000-0000-0000-000000000002','maps',now(),'openai','test',50,0,0.02),
 ('shared','00000000-0000-0000-0000-000000000002',NULL,now(),'openai','test',999,1,10);`)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewUsageRepository(db)
	filter := UsageFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour)}
	rows, err := repo.GetMCPServerUsage(context.Background(), &filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Server != "maps" || rows[0].Calls != 2 || rows[0].EstimatedUsageCalls != 1 || rows[0].ResultTokens != 150 || rows[0].Rereads != 2 || rows[0].AttributedCost != 0.03 {
		t.Fatalf("all: %+v", rows)
	}
	filter.MetadataFilters = []MetadataFilter{{Key: "team", Value: "B"}}
	rows, err = repo.GetMCPServerUsage(context.Background(), &filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Calls != 1 || rows[0].EstimatedUsageCalls != 0 || rows[0].AttributedCost != 0.02 {
		t.Fatalf("team B: %+v", rows)
	}
}
