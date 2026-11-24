package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	cleanupBatchSize = 64
	cleanupLease     = 5 * time.Minute
	cleanupPoll      = 15 * time.Second
	cleanupCoalesce  = 100 * time.Millisecond
	cleanupDBTimeout = 3 * time.Second
)

func (s *Service) wakeReportCleanup() {
	select {
	case s.cleanupWake <- struct{}{}:
	default:
	}
}

// RunReportCleanup runs one worker until cancellation. Startup and periodic
// polls recover work left behind by another process or a failed deletion.
func (s *Service) RunReportCleanup(ctx context.Context) {
	// Keep sampling the backlog even when an NFS unlink blocks the worker.
	pollingDone := make(chan struct{})
	go func() {
		defer close(pollingDone)
		s.pollReportCleanup(ctx)
	}()
	defer func() { <-pollingDone }()
	next := time.NewTimer(cleanupCoalesce)
	defer next.Stop()
	ready := next.C
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-s.cleanupWake:
			if ready == nil {
				next.Reset(cleanupCoalesce)
				ready = next.C
			}
		case <-ready:
			ready = nil
			n, err := s.processReportCleanup(ctx)
			if err != nil && ctx.Err() == nil {
				s.warnReportCleanup(ctx, "report cleanup batch failed", "error", err)
			}
			if err == nil && n == cleanupBatchSize {
				// Continue a backlog without imposing the upload wake-up delay.
				next.Reset(0)
				ready = next.C
			}
		}
	}
}

func (s *Service) pollReportCleanup(ctx context.Context) {
	poll := time.NewTicker(cleanupPoll)
	defer poll.Stop()
	for ctx.Err() == nil {
		s.recordCleanupBacklog(ctx)
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			s.wakeReportCleanup()
		}
	}
}

func (s *Service) recordCleanupBacklog(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, cleanupDBTimeout)
	count, age, err := s.db.ReportCleanupBacklog(queryCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			s.warnReportCleanup(ctx, "report cleanup backlog query failed", "error", err)
		}
		return
	}
	s.metrics.cleanupPending.Record(ctx, count)
	s.metrics.cleanupOldest.Record(ctx, age)
}

func (s *Service) processReportCleanup(ctx context.Context) (n int, err error) {
	ctx, span := s.tracer.Start(ctx, "Process report file cleanup")
	start := time.Now()
	defer func() {
		if n > 0 {
			s.metrics.cleanupDuration.Record(ctx, time.Since(start).Seconds())
		}
		telemetry.RecordError(span, err)
		span.End()
	}()
	token := uuid.NewString()
	dbCtx, cancel := context.WithTimeout(ctx, cleanupDBTimeout)
	work, err := s.db.ClaimReportCleanup(dbCtx, token, cleanupBatchSize, cleanupLease)
	cancel()
	if err != nil {
		return 0, err
	}
	var failures, firstRemoveErr error
	var failedPath string
	var failedCount int
	var completed []string
	for _, item := range work {
		if ctx.Err() != nil {
			break
		}
		removeErr := s.files.Remove(item.Path)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			s.metrics.cleanupAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("dscreg.report.cleanup.outcome", "success")))
			completed = append(completed, item.Path)
			continue
		}
		s.metrics.cleanupAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("dscreg.report.cleanup.outcome", "failure")))
		failedCount++
		if firstRemoveErr == nil {
			failedPath, firstRemoveErr = item.Path, removeErr
		}
		delay := min(5*time.Second<<min(item.Attempts-1, 6), 5*time.Minute)
		dbCtx, cancel := context.WithTimeout(ctx, cleanupDBTimeout)
		retryErr := s.db.RetryReportCleanup(dbCtx, token, item.Path, delay)
		cancel()
		failures = errors.Join(failures, retryErr)
	}
	if firstRemoveErr != nil {
		telemetry.RecordError(span, firstRemoveErr)
		s.warnReportCleanup(ctx, "report file cleanup failed", "path", failedPath, "error", firstRemoveErr, "batch_failures", failedCount)
	}
	// Persist completed work even if shutdown arrived during the last unlink.
	dbCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), cleanupDBTimeout)
	err = s.db.CompleteReportCleanup(dbCtx, token, completed)
	cancel()
	return len(work), errors.Join(failures, err)
}

// Limit warnings across a storage outage while counters capture every attempt.
func (s *Service) warnReportCleanup(ctx context.Context, message string, args ...any) {
	now := time.Now().Unix()
	last := s.cleanupLastWarning.Load()
	if now-last >= int64(cleanupPoll/time.Second) && s.cleanupLastWarning.CompareAndSwap(last, now) {
		s.logger.Log(ctx, slog.LevelWarn, message, args...)
	}
}
