package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
	return publication, err
}

func (s *Service) readContent(ctx context.Context, path string) ([]byte, error) {
	content, err := s.files.Read(ctx, path, s.maxReportBytes)
	return content, storageError(err)
}

// GetReport rechecks metadata after a storage error. This also handles a
// network filesystem invalidating an open handle during concurrent retirement.
func (s *Service) GetReport(ctx context.Context, id, reportID string, full bool) (database.ReportMetadata, []byte, error) {
	item, path, err := s.db.GetReport(ctx, id, reportID)
	if err != nil || !full {
		return item, nil, err
	}
	content, err := s.readContent(ctx, path)
	if err != nil {
		if _, _, lookupErr := s.db.GetReport(ctx, id, reportID); lookupErr != nil {
			return item, nil, lookupErr
		}
	}
	return item, content, err
}

func (s *Service) ReadReport(ctx context.Context, id, name string) ([]byte, error) {
	var failedID string
	var readErr error
	for {
		item, path, err := s.db.LatestConfigurationReport(ctx, id, name)
		if err != nil {
			return nil, err
		}
		if item.ID == failedID {
			return nil, readErr
		}
		content, err := s.readContent(ctx, path)
		if err == nil {
			return content, nil
		}
		failedID, readErr = item.ID, err
	}
}

func (s *Service) publishReport(ctx context.Context, id, name string, upload report.Upload) (database.Publication, error) {
	var publicationTime time.Duration
	defer func() { s.metrics.publish.Record(ctx, publicationTime.Seconds()) }()

	digest := sha256.Sum256(upload.Result)
	candidate, writeErr := s.files.Write(ctx, upload.Result)
	path := candidate
	if writeErr != nil {
		path = ""
	}
	var keepFile bool
	defer func() {
		if candidate != "" && !keepFile {
			// A canceled request must still record its unpublished file. Bound
			// the attempt if the database is unavailable.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			phaseCtx, span, start := s.databasePhase(cleanupCtx, "Enqueue unused report file")
			err := s.db.EnqueueReportCleanup(phaseCtx, candidate)
			finishDatabasePhase(span, start, &publicationTime, err)
			if err != nil {
				s.warnReportCleanup(phaseCtx, "enqueue unused report file failed", "path", candidate, "error", err)
			} else {
				s.wakeReportCleanup()
			}
		}
	}()

	var verifiedID, failedReadID string
	var readErr error
	for {
		commitCtx, span, start := s.databasePhase(ctx, "Publish metadata")
		result, err := s.db.CommitReport(commitCtx, id, name, upload, path, digest[:], verifiedID)
		keepFile = result.KeepFile
		phaseErr := err
		if writeErr != nil && errors.Is(err, database.ErrReportNotFound) {
			phaseErr = nil
			err = storageError(writeErr)
		}
		finishDatabasePhase(span, start, &publicationTime, phaseErr)
		if err != nil {
			return database.Publication{}, err
		}
		if result.LegacyPath == "" {
			if result.Outcome == database.MutationUpdated {
				s.wakeReportCleanup()
			}
			return result.Publication, nil
		}

		if result.Report.ID == failedReadID {
			return database.Publication{}, readErr
		}
		content, err := s.readContent(ctx, result.LegacyPath)
		if err != nil {
			// The legacy receipt may have retired while storage was being read.
			failedReadID, readErr = result.Report.ID, err
		} else {
			if sha256.Sum256(content) != digest {
				return database.Publication{}, database.ErrUploadConflict
			}
			verifiedID = result.Report.ID
		}
		s.tracePublicationRetry(ctx)
	}
}
