package reportstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProbeAndCleanup(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for range 3 {
		if err := store.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatalf("probe left files: %v, error=%v", files, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("closed root accepted")
	}
}

func TestInvalidRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "missing"), file} {
		if store, err := Open(context.Background(), path); store != nil || err == nil {
			t.Fatalf("invalid root %q accepted: %v", path, err)
		}
	}
}

func TestAcceptSymlinkAndSymlinkParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	for _, path := range []string{link, filepath.Join(link, "child")} {
		store, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("symlink root %q rejected: %v", path, err)
		}
		defer store.Close()
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || store.path != canonical {
			t.Fatalf("root was not canonicalized: got=%q, want=%q, error=%v", store.path, canonical, err)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatalf("symlink root %q is not ready: %v", path, err)
		}
	}
}

func TestRetargetedSymlinkDoesNotRedirectStorage(t *testing.T) {
	parent := t.TempDir()
	first, second := filepath.Join(parent, "first"), filepath.Join(parent, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(parent, "reports")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	store, err := Open(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("retargeting an alias invalidated the original root: %v", err)
	}
	original, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := store.root.Stat(".")
	if err != nil || !os.SameFile(original, opened) {
		t.Fatalf("storage was redirected: %v", err)
	}
	name, err := store.Write(context.Background(), []byte(`{"original":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first, name)); err != nil {
		t.Fatal("report did not use original root", err)
	}
	entries, err := os.ReadDir(second)
	if err != nil || len(entries) != 0 {
		t.Fatal("report write followed retargeted symlink", err)
	}
}

func TestReplacedRootIsNotReady(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "reports")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Rename(path, filepath.Join(parent, "old")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("open directory cannot be moved on this Windows filesystem: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("replacement root accepted")
	}
}

func TestUnwritableRoot(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires a non-root POSIX permission model; Windows uses ACLs")
	}
	path := t.TempDir()
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o700)
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("unwritable root accepted")
	}
}
