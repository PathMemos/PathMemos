package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewLoggerWritesToFile：LOG_FILE 非空时日志同时落盘（容器重建后仍可回溯）。
func TestNewLoggerWritesToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "app.log")
	t.Setenv("LOG_FILE", path)

	logger := newLogger("INFO")
	logger.Info("file-log-marker")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "file-log-marker") {
		t.Fatalf("log file missing marker, got %q", data)
	}
}

// TestNewLoggerFallsBackToStdout：LOG_FILE 不可用时不得 panic，降级为仅 stdout。
func TestNewLoggerFallsBackToStdout(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("LOG_FILE", filepath.Join(blocker, "app.log"))

	logger := newLogger("INFO")
	if logger == nil {
		t.Fatal("expected non-nil logger on fallback")
	}
	logger.Info("fallback-marker") // 不 panic 即通过
}
