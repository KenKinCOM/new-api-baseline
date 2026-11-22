package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// Log rotation and retention.
//
// Why this exists (T4 fact table §3.4): the historical logger opened
// logs/oneapi-<timestamp>.log on every start and never rotated, pruned or
// capped it. A long-lived process therefore grows a single file until the data
// disk is full, at which point the whole service stops writing (and, with SQLite
// on the same volume, stops working). Filenames are timestamped, so a restart
// "rotated" by accident — that is not a rotation policy.
//
// This writer adds the missing policy:
//   - size cap per file      (LOG_MAX_SIZE_MB, default 200)
//   - retention by age       (LOG_RETENTION_DAYS, default 14)
//   - bounded backup count   (LOG_MAX_BACKUPS, default 0 = unlimited by count)
//
// All knobs are optional; defaults keep a single node from filling its disk
// within days while staying close to the previous behaviour.

const (
	envLogMaxSizeMB      = "LOG_MAX_SIZE_MB"
	envLogRetentionDays  = "LOG_RETENTION_DAYS"
	envLogMaxBackups     = "LOG_MAX_BACKUPS"
	defaultLogMaxSizeMB  = 200
	defaultRetentionDays = 14
	logFilePrefix        = "oneapi-"
	logFileSuffix        = ".log"
)

// RotatingFileWriter is an io.Writer that appends to a timestamped file inside a
// directory, rotates it once the size cap is reached, and prunes expired files.
// It is safe for concurrent use.
type RotatingFileWriter struct {
	mu        sync.Mutex
	dir       string
	maxBytes  int64
	retention time.Duration
	maxFiles  int
	file      *os.File
	size      int64
	path      string
}

// NewRotatingFileWriter opens the first log file inside dir and prunes expired
// ones. The caller owns the writer and must Close it.
func NewRotatingFileWriter(dir string) (*RotatingFileWriter, error) {
	if dir == "" {
		return nil, fmt.Errorf("log directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &RotatingFileWriter{
		dir:       dir,
		maxBytes:  int64(envInt(envLogMaxSizeMB, defaultLogMaxSizeMB)) * 1024 * 1024,
		retention: time.Duration(envInt(envLogRetentionDays, defaultRetentionDays)) * 24 * time.Hour,
		maxFiles:  envInt(envLogMaxBackups, 0),
	}
	if w.maxBytes <= 0 {
		w.maxBytes = int64(defaultLogMaxSizeMB) * 1024 * 1024
	}
	if err := w.openNewFile(); err != nil {
		return nil, err
	}
	w.prune()
	return w, nil
}

// Path returns the currently active log file path.
func (w *RotatingFileWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// Write appends p, rotating first when the size cap would be exceeded. A single
// oversized write still succeeds: it is written to a fresh file so callers never
// lose a log line because of the cap.
func (w *RotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.openNewFileLocked(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the active file.
func (w *RotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingFileWriter) openNewFile() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.openNewFileLocked()
}

func (w *RotatingFileWriter) openNewFileLocked() error {
	path, err := w.nextPath()
	if err != nil {
		return err
	}
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	info, statErr := fd.Stat()
	var size int64
	if statErr == nil {
		size = info.Size()
	}
	w.file = fd
	w.path = path
	w.size = size
	return nil
}

func (w *RotatingFileWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	if err := w.openNewFileLocked(); err != nil {
		return err
	}
	w.pruneLocked()
	return nil
}

// nextPath builds oneapi-<timestamp>.log, adding a counter when a file with the
// same second already exists (rotation can happen twice inside one second).
func (w *RotatingFileWriter) nextPath() (string, error) {
	base := logFilePrefix + time.Now().Format("20060102150405")
	for i := 1; i <= 1000; i++ {
		name := base + logFileSuffix
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", base, i, logFileSuffix)
		}
		path := filepath.Join(w.dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("failed to allocate a log file name in %s", w.dir)
}

func (w *RotatingFileWriter) prune() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
}

// pruneLocked deletes expired log files and enforces LOG_MAX_BACKUPS, always
// keeping the file that is currently being written.
func (w *RotatingFileWriter) pruneLocked() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var logs []candidate
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, logFilePrefix) || !strings.HasSuffix(name, logFileSuffix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		path := filepath.Join(w.dir, name)
		if path == w.path {
			continue
		}
		if w.retention > 0 && now.Sub(info.ModTime()) > w.retention {
			_ = os.Remove(path)
			continue
		}
		logs = append(logs, candidate{path: path, modTime: info.ModTime()})
	}
	if w.maxFiles > 0 && len(logs)+1 > w.maxFiles {
		sort.Slice(logs, func(i, j int) bool { return logs[i].modTime.After(logs[j].modTime) })
		for _, item := range logs[w.maxFiles-1:] {
			_ = os.Remove(item.path)
		}
	}
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		common.SysError(fmt.Sprintf("invalid %s=%q, using %d", name, raw, fallback))
		return fallback
	}
	return value
}
