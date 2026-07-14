package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExecutable creates an executable stub file and returns its path.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	return path
}

func TestResolveClaudeBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit probing is Unix-specific")
	}

	t.Run("explicit path to an executable is honoured", func(t *testing.T) {
		path := writeExecutable(t, t.TempDir(), "claude")
		got, err := resolveClaudeBin(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != path {
			t.Fatalf("got %q, want %q", got, path)
		}
	})

	t.Run("explicit path that is not executable fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing")
		if _, err := resolveClaudeBin(path); err == nil {
			t.Fatal("expected an error for a non-existent explicit path")
		}
	})

	t.Run("bare name is found on PATH", func(t *testing.T) {
		dir := t.TempDir()
		want := writeExecutable(t, dir, "claude")
		t.Setenv("PATH", dir)
		got, err := resolveClaudeBin("claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("bare name is found in a probed home directory when PATH misses it", func(t *testing.T) {
		home := t.TempDir()
		localBin := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(localBin, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		want := writeExecutable(t, localBin, "claude")

		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir()) // an empty dir: claude is not on PATH
		got, err := resolveClaudeBin("claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("missing everywhere fails with a directory list", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", t.TempDir())
		// A name that cannot exist in PATH or the system probe dirs, so the
		// hardcoded /opt/homebrew and /usr/local candidates can't satisfy it.
		_, err := resolveClaudeBin("claude-does-not-exist-valyrium-test")
		if err == nil {
			t.Fatal("expected an error when claude is nowhere")
		}
		if !strings.Contains(err.Error(), "CLAUDE_GATEWAY_BIN") {
			t.Fatalf("error should point at the override, got: %v", err)
		}
	})
}
