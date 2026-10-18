package reportstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Store struct {
	path string
	root *os.Root
}

func Open(ctx context.Context, path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve report root: %w", err)
	}
	if err := validateParents(absolute); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open report root: %w", err)
	}
	s := &Store{path: absolute, root: root}
	if err := s.Check(ctx); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.root.Close()
}

func validateParents(path string) error {
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect report root: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("report root and its parents must be directories without symlinks")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func (s *Store) Check(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateParents(s.path); err != nil {
		return err
	}
	current, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("stat configured report root: %w", err)
	}
	opened, err := s.root.Stat(".")
	if err != nil {
		return fmt.Errorf("stat opened report root: %w", err)
	}
	if !os.SameFile(current, opened) {
		return errors.New("configured report root was replaced")
	}
	name := ".dscregd-probe-" + rand.Text()
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create report probe: %w", err)
	}
	defer func() { err = errors.Join(err, s.root.Remove(name)) }()
	_, writeErr := file.WriteString("dscregd\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write report probe: %w", err)
	}
	data, err := s.root.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read report probe: %w", err)
	}
	if string(data) != "dscregd\n" {
		return errors.New("report probe content mismatch")
	}
	return ctx.Err()
}
