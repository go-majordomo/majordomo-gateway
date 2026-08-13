package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/storage"
)

func TestResolveFilter(t *testing.T) {
	t.Run("default preset spans 30 days", func(t *testing.T) {
		f, err := resolveFilter(models.UsageQuery{})
		if err != nil {
			t.Fatalf("resolveFilter: %v", err)
		}
		if got := f.End.Sub(f.Start).Hours() / 24; got != 30 {
			t.Fatalf("expected a 30-day window, got %v days", got)
		}
	})

	t.Run("7d preset spans 7 days", func(t *testing.T) {
		f, err := resolveFilter(models.UsageQuery{Preset: "7d"})
		if err != nil {
			t.Fatalf("resolveFilter: %v", err)
		}
		if got := f.End.Sub(f.Start).Hours() / 24; got != 7 {
			t.Fatalf("expected a 7-day window, got %v days", got)
		}
	})

	t.Run("invalid status_class is a validation error", func(t *testing.T) {
		_, err := resolveFilter(models.UsageQuery{StatusClass: "nope"})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation, got %v", err)
		}
	})

	t.Run("more than two metadata filters is a validation error", func(t *testing.T) {
		_, err := resolveFilter(models.UsageQuery{MetadataFilters: []models.UsageQueryFilter{
			{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"},
		}})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation, got %v", err)
		}
	})

	t.Run("invalid api_key_id is a validation error", func(t *testing.T) {
		_, err := resolveFilter(models.UsageQuery{APIKeyID: "not-a-uuid"})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation, got %v", err)
		}
	})

	t.Run("explicit start/end round-trip", func(t *testing.T) {
		f, err := resolveFilter(models.UsageQuery{Start: "2026-01-01", End: "2026-01-08"})
		if err != nil {
			t.Fatalf("resolveFilter: %v", err)
		}
		if got := f.End.Sub(f.Start).Hours() / 24; got != 7 {
			t.Fatalf("expected a 7-day explicit window, got %v days", got)
		}
	})
}

// fakeUsageStore implements repositories.UsageStorage; only GetRequestDetail is used.
type fakeUsageStore struct {
	repositories.UsageStorage
	detail    *models.RequestLog
	detailErr error
}

func (f *fakeUsageStore) GetRequestDetail(_ context.Context, _ uuid.UUID) (*models.RequestLog, error) {
	return f.detail, f.detailErr
}

// stubBodyStore implements storage.Store; only Download is used.
type stubBodyStore struct {
	storage.Store
	content *storage.BodyContent
}

func (s stubBodyStore) Download(_ context.Context, _ string) (*storage.BodyContent, error) {
	return s.content, nil
}

func TestUsageService_RequestBody(t *testing.T) {
	id := uuid.New()

	t.Run("archival disabled", func(t *testing.T) {
		svc := NewUsageService(&fakeUsageStore{}, nil)
		_, err := svc.RequestBody(context.Background(), id)
		if !errors.Is(err, ErrBodyArchivalDisabled) {
			t.Fatalf("expected ErrBodyArchivalDisabled, got %v", err)
		}
	})

	t.Run("request not found bubbles", func(t *testing.T) {
		store := &fakeUsageStore{detailErr: repositories.ErrRequestNotFound}
		svc := NewUsageService(store, stubBodyStore{})
		_, err := svc.RequestBody(context.Background(), id)
		if !errors.Is(err, repositories.ErrRequestNotFound) {
			t.Fatalf("expected ErrRequestNotFound, got %v", err)
		}
	})

	t.Run("no body archived", func(t *testing.T) {
		store := &fakeUsageStore{detail: &models.RequestLog{}}
		svc := NewUsageService(store, stubBodyStore{})
		_, err := svc.RequestBody(context.Background(), id)
		if !errors.Is(err, ErrNoBodyArchived) {
			t.Fatalf("expected ErrNoBodyArchived, got %v", err)
		}
	})

	t.Run("downloads archived body", func(t *testing.T) {
		key := "requests/" + id.String()
		want := &storage.BodyContent{RequestID: id.String()}
		store := &fakeUsageStore{detail: &models.RequestLog{BodyS3Key: &key}}
		svc := NewUsageService(store, stubBodyStore{content: want})
		got, err := svc.RequestBody(context.Background(), id)
		if err != nil {
			t.Fatalf("RequestBody: %v", err)
		}
		if got != want {
			t.Fatalf("expected downloaded content, got %+v", got)
		}
	})
}
