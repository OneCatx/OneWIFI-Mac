package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareUnixSocketPathRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important-data")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := prepareUnixSocketPath(path); err == nil {
		t.Fatal("expected regular file to be rejected")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("regular file was removed: %v", err)
	}
	if string(contents) != "keep" {
		t.Fatalf("regular file changed: %q", contents)
	}
}

func TestPrepareUnixSocketPathRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "socket-link")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := prepareUnixSocketPath(link); err == nil {
		t.Fatal("expected symlink to be rejected")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
}

func TestPrepareUnixSocketPathRemovesStaleSocket(t *testing.T) {
	// macOS sockaddr_un paths are short; t.TempDir() lives under a long
	// per-user path and can fail before the behavior under test is reached.
	directory, err := os.MkdirTemp("/tmp", "prs-sock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "stale.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	if err := prepareUnixSocketPath(path); err != nil {
		t.Fatalf("remove stale socket: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket still exists: %v", err)
	}
}
