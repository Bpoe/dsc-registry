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

func (s *Service) PublishReport(ctx context.Context, id string, body []byte) (database.Publication, error) {
	upload, err := report.Parse(body)
	if err != nil {
		return database.Publication{}, err
	}
	publication, err := s.publishReport(ctx, id, upload.Name, upload)
	if err == nil {
		data := eventing.Identifiers{MachineID: id, ConfigurationName: upload.Name, ReportID: publication.Report.ID}
		switch publication.Outcome {
		case database.MutationCreated:
			s.events.Notify(eventing.ReportCreated, data)
		case database.MutationUpdated:
			s.events.Notify(eventing.ReportUpdated, data)
		}
	}
	s.removeReportFiles(publication.CleanupPaths)
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
	start := time.Now()
	path, err := s.files.Write(ctx, content)
	var cleanupErr *reportstore.CleanupError
	if errors.As(err, &cleanupErr) {
		s.logger.Warn("report file cleanup failed", "path", cleanupErr.Path, "error", cleanupErr.Err)
	}
	outcome := "written"
	if err != nil {
		outcome = "write_error"
	}
	s.logger.Info("report write", "outcome", outcome, "duration", time.Since(start))
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
	defer func() { publication.CleanupPaths = append(publication.CleanupPaths, unused...) }()
	digest := sha256.Sum256(upload.Result)
	for {
		existingID, revision, existingPath, storedDigest, err := s.db.FindReportUpload(ctx, id, name, upload.ID)
		path := ""
		switch {
		case err == nil:
			if revision != upload.ConfigurationRevision {
				return database.Publication{}, database.ErrUploadConflict
			}
			if storedDigest == nil {
				content, err := s.readContent(ctx, existingPath)
				if errors.Is(err, os.ErrNotExist) {
					currentID, _, _, _, lookupErr := s.db.FindReportUpload(ctx, id, name, upload.ID)
					if lookupErr != nil && !errors.Is(lookupErr, database.ErrReportNotFound) {
						return database.Publication{}, lookupErr
					}
					if currentID != existingID {
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
		publication, keepFile, err := s.db.CommitReport(ctx, id, name, upload, path, digest[:], existingID)
		if path != "" && !keepFile {
			unused = append(unused, path)
		}
		if errors.Is(err, database.ErrReportChanged) {
			// A retry's report was replaced after the preliminary lookup.
			// Recheck it without holding a transaction during file I/O.
			continue
		}
		return publication, err
	}
}
