// Package requestlog provides async writing of request logs to PostgreSQL.
package requestlog

import (
	"context"
	"log/slog"

	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// Writer asynchronously writes request logs to PostgreSQL. It records every metadata
// key it sees (feeding a HyperLogLog cardinality estimate) but only copies *activated*
// keys into indexed_metadata, so the GIN index stays bounded no matter what callers send.
type Writer struct {
	db             *sqlx.DB
	logChan        chan *models.RequestLog
	done           chan struct{}
	activeKeyCache *repositories.ActiveKeysCache
	hllManager     *repositories.HLLManager
}

// New constructs a Writer and starts the background write loop.
func New(db *sqlx.DB, activeKeyCache *repositories.ActiveKeysCache, hllManager *repositories.HLLManager) *Writer {
	w := &Writer{
		db:             db,
		logChan:        make(chan *models.RequestLog, 1000),
		done:           make(chan struct{}),
		activeKeyCache: activeKeyCache,
		hllManager:     hllManager,
	}
	go w.writeLoop()
	return w
}

// WriteRequestLog enqueues a request log for async writing. Non-blocking: drops
// the log with a warning if the channel is full.
func (w *Writer) WriteRequestLog(_ context.Context, log *models.RequestLog) {
	select {
	case w.logChan <- log:
	default:
		slog.Warn("request log channel full, dropping log", "request_id", log.ID)
	}
}

// Ping verifies database connectivity. Satisfies server.HealthChecker.
func (w *Writer) Ping(ctx context.Context) error {
	return w.db.PingContext(ctx)
}

// Close drains the write channel, flushes HLL state, and closes the database.
func (w *Writer) Close() error {
	close(w.done)
	if w.hllManager != nil {
		w.hllManager.Stop()
	}
	return w.db.Close()
}

func (w *Writer) writeLoop() {
	for {
		select {
		case log := <-w.logChan:
			w.writeLog(log)
		case <-w.done:
			for len(w.logChan) > 0 {
				w.writeLog(<-w.logChan)
			}
			return
		}
	}
}

func (w *Writer) writeLog(log *models.RequestLog) {
	ctx := context.Background()

	_, err := repositories.InsertRequest(ctx, w.db, w.activeKeyCache.ForRequest(ctx, log.MajordomoAPIKeyID), log, false)
	if err != nil {
		slog.Error("failed to write request log", "error", err, "request_id", log.ID)
		return
	}

	repositories.RegisterRequestMetadata(ctx, w.db, w.hllManager, []*models.RequestLog{log})
}
