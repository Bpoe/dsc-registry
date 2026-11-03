package reportstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/google/uuid"
)

// The file seam exercises partial writes, sync and close failures without
// replacing the confinement and exclusive-create behavior.
type reportFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// CleanupError identifies a failed removal without obscuring the write failure.
type CleanupError struct {
	Path string
	Err  error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("remove partial report %s: %v", e.Path, e.Err)
}
func (e *CleanupError) Unwrap() error { return e.Err }

func (s *Store) Write(ctx context.Context, content []byte) (string, error) {
	return s.write(ctx, content, func(name string) (reportFile, error) {
		return s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	})
}

func (s *Store) write(ctx context.Context, content []byte, create func(string) (reportFile, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name := uuid.NewString() + ".json"
	file, err := create(name)
	if err != nil {
		return "", fmt.Errorf("create report: %w", err)
	}
	n, writeErr := file.Write(content)
	if writeErr == nil && n != len(content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		writeErr = fmt.Errorf("write report: %w", writeErr)
	}
	err = errors.Join(writeErr, ctx.Err())
	if err == nil {
		if syncErr := file.Sync(); syncErr != nil {
			err = fmt.Errorf("sync report: %w", syncErr)
		}
	}
	if closeErr := file.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close report: %w", closeErr))
	}
	err = errors.Join(err, ctx.Err())
	if err != nil {
		if cleanupErr := s.Remove(name); cleanupErr != nil {
			err = errors.Join(err, &CleanupError{Path: name, Err: cleanupErr})
		}
		return "", err
	}
	return name, nil
}

func validPath(name string) bool {
	stem, ok := strings.CutSuffix(name, ".json")
	id, err := uuid.Parse(stem)
	return ok && err == nil && id.String() == stem
}

func (s *Store) Read(ctx context.Context, name string, limit int64) ([]byte, error) {
	if !validPath(name) {
		return nil, errors.New("invalid report storage key")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open report: %w", err)
	}
	info, statErr := file.Stat()
	if statErr == nil && (!info.Mode().IsRegular() || info.Size() > limit) {
		statErr = errors.New("invalid stored report")
	}
	var content []byte
	if statErr == nil {
		readLimit := limit
		if readLimit < math.MaxInt64 {
			readLimit++
		}
		content, err = io.ReadAll(io.LimitReader(file, readLimit))
		if int64(len(content)) > limit {
			err = errors.Join(err, errors.New("stored report exceeds limit"))
		}
	}
	if statErr != nil {
		statErr = fmt.Errorf("stat report: %w", statErr)
	}
	if err != nil {
		err = fmt.Errorf("read report: %w", err)
	}
	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close report: %w", closeErr)
	}
	return content, errors.Join(statErr, err, closeErr, ctx.Err())
}

// Remove reclaims an unpublished file or a key retired by a successful commit.
// Keys are never reused; another replica may already have removed the file.
func (s *Store) Remove(name string) error {
	if !validPath(name) {
		return errors.New("invalid report storage key")
	}
	err := s.root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete report: %w", err)
	}
	return nil
}
