package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeUntilRotation drives the writer past its size cap and returns the paths
// that were created.
func activeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), logFilePrefix) && strings.HasSuffix(entry.Name(), logFileSuffix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestRotatingFileWriterRotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envLogMaxSizeMB, "1") // 1 MiB cap keeps the test fast
	w, err := NewRotatingFileWriter(dir)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil {
			t.Fatalf("close: %v", closeErr)
		}
	}()

	first := w.Path()
	chunk := []byte(strings.Repeat("x", 256*1024) + "\n") // 256 KiB
	for i := 0; i < 6; i++ {                              // 1.5 MiB total -> at least one rotation
		if _, writeErr := w.Write(chunk); writeErr != nil {
			t.Fatalf("write %d: %v", i, writeErr)
		}
	}

	if w.Path() == first {
		t.Fatalf("expected rotation to a new file, still on %s", first)
	}
	files := activeFiles(t, dir)
	if len(files) < 2 {
		t.Fatalf("expected >=2 log files after rotation, got %v", files)
	}
	for _, name := range files {
		info, statErr := os.Stat(filepath.Join(dir, name))
		if statErr != nil {
			t.Fatalf("stat %s: %v", name, statErr)
		}
		// Rotation happens before the write that would cross the cap, so a file
		// may overshoot by at most one chunk (the write that filled the last gap).
		if info.Size() > w.maxBytes+int64(len(chunk)) {
			t.Fatalf("file %s grew to %d bytes, cap %d not enforced", name, info.Size(), w.maxBytes)
		}
	}
	// Current path must exist and be tracked.
	if _, statErr := os.Stat(w.Path()); statErr != nil {
		t.Fatalf("current log path missing: %v", statErr)
	}
}

func TestRotatingFileWriterPrunesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, logFilePrefix+"20000101000000"+logFileSuffix)
	if err := os.WriteFile(stale, []byte("old\n"), 0o640); err != nil {
		t.Fatalf("seed stale log: %v", err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	t.Setenv(envLogRetentionDays, "14")
	w, err := NewRotatingFileWriter(dir)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	if _, statErr := os.Stat(stale); !os.IsNotExist(statErr) {
		t.Fatalf("expired log %s was not pruned (err=%v)", stale, statErr)
	}
	if _, statErr := os.Stat(w.Path()); statErr != nil {
		t.Fatalf("active log missing after prune: %v", statErr)
	}
}

func TestRotatingFileWriterKeepsActiveFileWhenRetentionZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envLogRetentionDays, "0") // 0 disables age-based pruning
	t.Setenv(envLogMaxBackups, "1")
	w, err := NewRotatingFileWriter(dir)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	if _, statErr := os.Stat(w.Path()); statErr != nil {
		t.Fatalf("active log missing: %v", statErr)
	}
}
