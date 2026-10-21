package reportstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirectPrivateReports(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	content := []byte("{ \"opaque\":9007199254740993123456789 }\n")
	first, err := store.Write(context.Background(), content)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Write(context.Background(), content)
	if err != nil || first == second || !validPath(first) || !validPath(second) {
		t.Fatal("report keys not unique relative UUIDs", err)
	}
	got, err := store.Read(context.Background(), first, int64(len(content)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatal("original bytes changed", err)
	}
	info, err := os.Stat(filepath.Join(root, first))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatal("report is not private", err)
	}
	if _, err := store.Read(context.Background(), first, int64(len(content)-1)); err == nil {
		t.Fatal("stored size bound ignored")
	}
	for _, name := range []string{"../escape.json", `..\escape.json`, filepath.Join(root, first), "report.json"} {
		if _, err := store.Read(context.Background(), name, 100); err == nil {
			t.Fatal("unsafe read accepted", name)
		}
		if err := store.Remove(name); err == nil {
			t.Fatal("unsafe remove accepted", name)
		}
	}
	if err := store.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first); err != nil {
		t.Fatal("retirement is not idempotent", err)
	}
}

type faultyFile struct {
	*os.File
	stage       string
	closeCalled bool
	writeErr    error
	afterClose  func() error
}

func (f *faultyFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		n, _ := f.File.Write(data[:len(data)/2])
		return n, f.writeErr
	}
	if f.stage == "write" {
		n, _ := f.File.Write(data[:len(data)/2])
		return n, errors.New("injected partial write")
	}
	if f.stage == "short" {
		return f.File.Write(data[:len(data)/2])
	}
	return f.File.Write(data)
}

func (f *faultyFile) Sync() error {
	if f.stage == "sync" {
		return errors.New("injected sync failure")
	}
	return f.File.Sync()
}

func (f *faultyFile) Close() error {
	f.closeCalled = true
	err := f.File.Close()
	if f.afterClose != nil {
		err = errors.Join(err, f.afterClose())
	}
	if f.stage == "close" {
		return errors.Join(err, errors.New("injected close failure"))
	}
	return err
}

func TestPartialWriteCleanupFailure(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	writeErr := errors.New("injected write failure")
	var path string
	key, err := store.write(context.Background(), []byte(`{"private":"report"}`), func(name string) (reportFile, error) {
		path = name
		file, err := store.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		return &faultyFile{File: file, writeErr: writeErr, afterClose: func() error {
			if err := store.root.Remove(name); err != nil {
				return err
			}
			if err := store.root.Mkdir(name, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, name, "blocker"), nil, 0o600)
		}}, nil
	})
	var cleanupErr *CleanupError
	if key != "" || !errors.Is(err, writeErr) || !errors.As(err, &cleanupErr) ||
		cleanupErr.Path != path || cleanupErr.Err == nil {
		t.Fatal("cleanup failure masked original write failure or lost path", key, err)
	}
}

func TestDirectWriteFaults(t *testing.T) {
	for _, stage := range []string{"write", "short", "sync", "close", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			store, err := Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var file *faultyFile
			key, err := store.write(ctx, []byte(`{"report":"secret"}`), func(name string) (reportFile, error) {
				actual, err := store.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return nil, err
				}
				// The final unique filename is visible during the write; there
				// is no temporary pathname or later rename.
				if _, err := os.Stat(filepath.Join(root, name)); err != nil {
					t.Fatal("direct destination missing", err)
				}
				file = &faultyFile{File: actual, stage: stage}
				if stage == "cancel" {
					cancel()
				}
				return file, nil
			})
			if err == nil || key != "" || file == nil || !file.closeCalled {
				t.Fatal("failed file exposed for publication or not closed", key, err)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 {
				t.Fatal("failed write not reclaimed", files, err)
			}
		})
	}
}

func TestReportSymlinkConfinement(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	store, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, err := store.Write(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(key); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "secret.json")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, key)); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	if _, err := store.Read(context.Background(), key, 100); err == nil {
		t.Fatal("report escaped root")
	}
	if err := store.Remove(key); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "outside" {
		t.Fatal("retirement followed symlink", err)
	}
}
