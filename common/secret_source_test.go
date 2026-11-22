package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_secret")
	if err := os.WriteFile(path, []byte("file-value\n"), 0o400); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	t.Setenv("TEST_SECRET_FILE", path)
	t.Setenv("TEST_SECRET", "")

	value, origin, err := LoadSecret("TEST_SECRET")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value != "file-value" {
		t.Fatalf("expected trimmed file value, got %q", value)
	}
	if origin != SecretOriginFile {
		t.Fatalf("expected file origin, got %s", origin)
	}
}

func TestLoadSecretFallsBackToEnv(t *testing.T) {
	t.Setenv("TEST_SECRET_FILE", "")
	t.Setenv("TEST_SECRET", "env-value")

	value, origin, err := LoadSecret("TEST_SECRET")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value != "env-value" || origin != SecretOriginEnv {
		t.Fatalf("expected env value/env origin, got %q/%s", value, origin)
	}
}

func TestLoadSecretFileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("from-file"), 0o400); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	t.Setenv("TEST_SECRET_FILE", path)
	t.Setenv("TEST_SECRET", "from-env")

	value, origin, err := LoadSecret("TEST_SECRET")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value != "from-file" || origin != SecretOriginFile {
		t.Fatalf("expected file to win, got %q/%s", value, origin)
	}
}

func TestLoadSecretUnset(t *testing.T) {
	t.Setenv("TEST_SECRET_FILE", "")
	t.Setenv("TEST_SECRET", "")

	value, origin, err := LoadSecret("TEST_SECRET")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value != "" || origin != SecretOriginUnset {
		t.Fatalf("expected unset, got %q/%s", value, origin)
	}
}

func TestLoadSecretEmptyFileFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("\n \n"), 0o400); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	t.Setenv("TEST_SECRET_FILE", path)
	t.Setenv("TEST_SECRET", "")

	if _, _, err := LoadSecret("TEST_SECRET"); err == nil {
		t.Fatal("expected an error for an empty secret file, got nil")
	}
}

func TestLoadSecretMissingFileFails(t *testing.T) {
	t.Setenv("TEST_SECRET_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("TEST_SECRET", "")

	if _, _, err := LoadSecret("TEST_SECRET"); err == nil {
		t.Fatal("expected an error for a missing secret file, got nil")
	}
}