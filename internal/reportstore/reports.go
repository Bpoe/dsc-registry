package reportstore

import (
	"context"
	"errors"
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
		return "", err
	}
	n, writeErr := file.Write(content)
	if writeErr == nil && n != len(content) {
		writeErr = io.ErrShortWrite
	}
	err = errors.Join(writeErr, ctx.Err())
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close(), ctx.Err())
	if err != nil {
		// A failed or interrupted write is never returned for SQL publication.
		// If removal fails, offline orphan cleanup can reclaim this unique file.
		return "", errors.Join(err, s.root.Remove(name))
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
		return nil, err
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
			err = errors.New("stored report exceeds limit")
		}
	}
	return content, errors.Join(statErr, err, file.Close(), ctx.Err())
}

// Remove is only called after SQL has confirmed that this never-reused key
// is unreferenced. It is also safe when another replica already retired it.
func (s *Store) Remove(name string) error {
	if !validPath(name) {
		return errors.New("invalid report storage key")
	}
	err := s.root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
