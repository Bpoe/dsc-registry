package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrReportStorage lets callers classify storage failures without inspecting
// filesystem errors. The underlying cause remains available through errors.Is.
var ErrReportStorage = errors.New("report storage unavailable")

func storageError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrReportStorage, err)
}

func (s *Service) PublishReport(ctx context.Context, id string, body []byte) (publication database.Publication, err error) {
	ctx, span := s.tracer.Start(ctx, "Service.PublishReport")
	defer span.End()
	start := time.Now()
	defer func() {
		outcome := "failed"
		if err == nil {
			outcome = "created"
			if publication.Outcome == database.MutationUnchanged {
				outcome = "duplicate"
			}
		}
		s.metrics.ingest.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("dscreg.report.ingest.outcome", outcome)))
		telemetry.RecordError(span, err)
	}()
	upload, err := report.Parse(body)
	if err != nil {
		return database.Publication{}, err
	}
	s.metrics.size.Record(ctx, int64(len(upload.Result)))
	publication, err = s.publishReport(ctx, id, upload.Name, upload)
	if err == nil {
		data := eventing.Identifiers{MachineID: id, ConfigurationName: upload.Name, ReportID: publication.Report.ID}
		switch publication.Outcome {
		case database.MutationCreated:
			s.events.Notify(eventing.ReportCreated, data)
		case database.MutationUpdated:
			s.events.Notify(eventing.ReportUpdated, data)
		}
	}
	if len(publication.CleanupPaths) != 0 {
		_, cleanupSpan := s.tracer.Start(ctx, "Cleanup retired reports")
		s.removeReportFiles(publication.CleanupPaths)
		cleanupSpan.End()
	}
	return publication, err
}

func (s *Service) removeReportFiles(paths []string) {
	for _, path := range paths {
		if err := s.files.Remove(path); err != nil {
			s.logger.Warn("report file cleanup failed", "path", path, "error", err)
		}
	}
}

func (s *Service) writeContent(ctx context.Context, content []byte) (string, error) {
	path, err := s.files.Write(ctx, content)
	var cleanupErr *reportstore.CleanupError
	if errors.As(err, &cleanupErr) {
		s.logger.Warn("report file cleanup failed", "path", cleanupErr.Path, "error", cleanupErr.Err)
	}
	return path, storageError(err)
}

func (s *Service) readContent(ctx context.Context, path string) ([]byte, error) {
	content, err := s.files.Read(ctx, path, s.maxReportBytes)
	return content, storageError(err)
}

// GetReport rechecks missing content after concurrent retirement. A retired ID
// is not found; an unchanged reference with missing content is a storage error.
func (s *Service) GetReport(ctx context.Context, id, reportID string, full bool) (database.ReportMetadata, []byte, error) {
	item, path, err := s.db.GetReport(ctx, id, reportID)
	if err != nil || !full {
		return item, nil, err
	}
	content, err := s.readContent(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		if _, _, lookupErr := s.db.GetReport(ctx, id, reportID); lookupErr != nil {
			return item, nil, lookupErr
		}
	}
	return item, content, err
}

func (s *Service) ReadReport(ctx context.Context, id, name string) ([]byte, error) {
	var missingID string
	var missingErr error
	for {
		item, path, err := s.db.LatestConfigurationReport(ctx, id, name)
		if err != nil {
			return nil, err
		}
		if item.ID == missingID {
			return nil, missingErr
		}
		content, err := s.readContent(ctx, path)
		if !errors.Is(err, os.ErrNotExist) {
			return content, err
		}
		missingID, missingErr = item.ID, err
	}
}

func (s *Service) publishReport(ctx context.Context, id, name string, upload report.Upload) (publication database.Publication, err error) {
	var unused []string
	var publicationTime time.Duration
	defer func() { s.metrics.publish.Record(ctx, publicationTime.Seconds()) }()
	defer func() { publication.CleanupPaths = append(publication.CleanupPaths, unused...) }()
	digest := sha256.Sum256(upload.Result)
	for {
		lookupCtx, lookupSpan, lookupStart := s.databasePhase(ctx, "Find existing upload")
		existingID, revision, existingPath, storedDigest, err := s.db.FindReportUpload(lookupCtx, id, name, upload.ID)
		lookupErr := err
		if errors.Is(lookupErr, database.ErrReportNotFound) {
			lookupErr = nil
		}
		finishDatabasePhase(lookupSpan, lookupStart, &publicationTime, lookupErr)
		path := ""
		switch {
		case err == nil:
			if revision != upload.ConfigurationRevision {
				return database.Publication{}, database.ErrUploadConflict
			}
			if storedDigest == nil {
				content, err := s.readContent(ctx, existingPath)
				if errors.Is(err, os.ErrNotExist) {
					recheckCtx, recheckSpan, recheckStart := s.databasePhase(ctx, "Find existing upload")
					currentID, _, _, _, lookupErr := s.db.FindReportUpload(recheckCtx, id, name, upload.ID)
					phaseErr := lookupErr
					if errors.Is(phaseErr, database.ErrReportNotFound) {
						phaseErr = nil
					}
					finishDatabasePhase(recheckSpan, recheckStart, &publicationTime, phaseErr)
					if lookupErr != nil && !errors.Is(lookupErr, database.ErrReportNotFound) {
						return database.Publication{}, lookupErr
					}
					if currentID != existingID {
						traceRetry(ctx)
						continue
					}
				}
				if err != nil {
					return database.Publication{}, err
				}
				oldDigest := sha256.Sum256(content)
				storedDigest = oldDigest[:]
			}
			if !bytes.Equal(storedDigest, digest[:]) {
				return database.Publication{}, database.ErrUploadConflict
			}
		case errors.Is(err, database.ErrReportNotFound):
			path, err = s.writeContent(ctx, upload.Result)
			if err != nil {
				return database.Publication{}, err
			}
		default:
			return database.Publication{}, err
		}
		commitCtx, commitSpan, commitStart := s.databasePhase(ctx, "Publish metadata")
		publication, keepFile, err := s.db.CommitReport(commitCtx, id, name, upload, path, digest[:], existingID)
		finishDatabasePhase(commitSpan, commitStart, &publicationTime, err)
		if path != "" && !keepFile {
			unused = append(unused, path)
		}
		if errors.Is(err, database.ErrReportChanged) {
			// A retry's report was replaced after the preliminary lookup.
			// Recheck it without holding a transaction during file I/O.
			traceRetry(ctx)
			continue
		}
		return publication, err
	}
}

func traceRetry(ctx context.Context) { trace.SpanFromContext(ctx).AddEvent("Report publication retry") }
