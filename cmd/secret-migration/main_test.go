package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	_ "github.com/glebarez/go-sqlite"
)

const testMasterKey = "migration-test-master-key"

type fixture struct {
	dsn     string
	path    string
	backups string
}

// newFixture builds a throwaway SQLite database shaped like the production
// tables that hold credentials: channels.key and the sensitive options.
func newFixture(t *testing.T) fixture {
	t.Helper()
	if err := common.SetSecretEncryptionKeyMaterial(testMasterKey); err != nil {
		t.Fatalf("install test key: %v", err)
	}
	t.Cleanup(func() { common.ResetSecretEncryptionKeyForTest() })

	dir := t.TempDir()
	path := filepath.Join(dir, "new-api.db")
	backups := filepath.Join(dir, "backups")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()

	statements := []string{
		"CREATE TABLE channels (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, `key` TEXT, status INTEGER DEFAULT 1)",
		"CREATE TABLE options (`key` TEXT PRIMARY KEY, value TEXT)",
		"INSERT INTO channels (name, `key`, status) VALUES ('openai', 'sk-openai-plain', 1)",
		"INSERT INTO channels (name, `key`, status) VALUES ('azure', 'azure-plain-key', 1)",
		"INSERT INTO channels (name, `key`, status) VALUES ('empty', '', 1)",
		"INSERT INTO options (`key`, value) VALUES ('SMTPToken', 'smtp-plain')",
		"INSERT INTO options (`key`, value) VALUES ('EpayKey', 'epay-plain')",
		"INSERT INTO options (`key`, value) VALUES ('StripeApiSecret', '')",
		"INSERT INTO options (`key`, value) VALUES ('Logo', 'logo-plain.png')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return fixture{dsn: "sqlite:" + path, path: path, backups: backups}
}

func (f fixture) query(t *testing.T, query string, args ...any) string {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatalf("open for query: %v", err)
	}
	defer db.Close()
	var value sql.NullString
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value.String
}

func (f fixture) channelKey(t *testing.T, name string) string {
	t.Helper()
	return f.query(t, "SELECT `key` FROM channels WHERE name = ?", name)
}

func (f fixture) optionValue(t *testing.T, key string) string {
	t.Helper()
	return f.query(t, "SELECT value FROM options WHERE `key` = ?", key)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	f := newFixture(t)

	if err := run(f.dsn, "encrypt", "", f.backups, "", false); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	for _, name := range []string{"openai", "azure"} {
		stored := f.channelKey(t, name)
		if !common.IsEncryptedSecret(stored) {
			t.Fatalf("channel %s key was not encrypted: %q", name, stored)
		}
	}
	for _, key := range []string{"SMTPToken", "EpayKey"} {
		if stored := f.optionValue(t, key); !common.IsEncryptedSecret(stored) {
			t.Fatalf("option %s was not encrypted: %q", key, stored)
		}
	}
	if got := f.optionValue(t, "Logo"); got != "logo-plain.png" {
		t.Fatalf("non-sensitive option must not be encrypted, got %q", got)
	}
	if got := f.channelKey(t, "empty"); got != "" {
		t.Fatalf("empty channel key must stay empty, got %q", got)
	}
	if got := f.optionValue(t, "StripeApiSecret"); got != "" {
		t.Fatalf("empty option must stay empty, got %q", got)
	}

	backups, err := filepath.Glob(filepath.Join(f.backups, "*.jsonl.gz"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected exactly one backup, got %v (%v)", backups, err)
	}

	if err := run(f.dsn, "decrypt", "", f.backups, "", false); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got := f.channelKey(t, "openai"); got != "sk-openai-plain" {
		t.Fatalf("rollback did not restore the plaintext key, got %q", got)
	}
	if got := f.optionValue(t, "SMTPToken"); got != "smtp-plain" {
		t.Fatalf("rollback did not restore the plaintext option, got %q", got)
	}
}

func TestEncryptIsIdempotent(t *testing.T) {
	f := newFixture(t)
	if err := run(f.dsn, "encrypt", "", f.backups, "", false); err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	before := f.channelKey(t, "openai")
	if err := run(f.dsn, "encrypt", "", f.backups, "", false); err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if after := f.channelKey(t, "openai"); after != before {
		t.Fatal("re-running the migration must not re-encrypt an existing envelope")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	if err := run(f.dsn, "encrypt", "", f.backups, "", true); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if got := f.channelKey(t, "openai"); got != "sk-openai-plain" {
		t.Fatalf("dry-run changed the row: %q", got)
	}
}

func TestStaleBackupIsRejected(t *testing.T) {
	f := newFixture(t)
	if err := run(f.dsn, "backup", "", f.backups, "", false); err != nil {
		t.Fatalf("backup: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(f.backups, "*.jsonl.gz"))
	if len(backups) != 1 {
		t.Fatalf("expected one backup, got %v", backups)
	}

	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("INSERT INTO channels (name, `key`) VALUES ('late', 'late-key')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	err = run(f.dsn, "encrypt", backups[0], "", "", false)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected a stale-backup rejection, got %v", err)
	}
	if got := f.channelKey(t, "openai"); got != "sk-openai-plain" {
		t.Fatalf("a rejected run must not touch rows, got %q", got)
	}
}

func TestEncryptRefusesWithoutBackup(t *testing.T) {
	f := newFixture(t)
	err := run(f.dsn, "encrypt", "", "", "", false)
	if err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("expected a missing-backup refusal, got %v", err)
	}
	if got := f.channelKey(t, "openai"); got != "sk-openai-plain" {
		t.Fatalf("refused run changed a row: %q", got)
	}
}

func TestDecryptWithWrongKeyAborts(t *testing.T) {
	f := newFixture(t)
	if err := run(f.dsn, "encrypt", "", f.backups, "", false); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	encrypted := f.channelKey(t, "openai")

	if err := common.SetSecretEncryptionKeyMaterial("a-different-master-key"); err != nil {
		t.Fatalf("switch key: %v", err)
	}
	err := run(f.dsn, "decrypt", "", f.backups, "", false)
	if err == nil {
		t.Fatal("decrypting with the wrong key must fail")
	}
	if got := f.channelKey(t, "openai"); got != encrypted {
		t.Fatalf("failed rollback must leave rows untouched, got %q", got)
	}
}

func TestRestoreFromBackup(t *testing.T) {
	f := newFixture(t)
	if err := run(f.dsn, "backup", "", f.backups, "", false); err != nil {
		t.Fatalf("backup: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(f.backups, "*.jsonl.gz"))
	if len(backups) != 1 {
		t.Fatalf("expected one backup, got %v", backups)
	}
	if err := run(f.dsn, "encrypt", backups[0], "", "", false); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !common.IsEncryptedSecret(f.channelKey(t, "openai")) {
		t.Fatal("expected an envelope before restore")
	}
	if err := run(f.dsn, "restore", backups[0], "", "", false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := f.channelKey(t, "openai"); got != "sk-openai-plain" {
		t.Fatalf("restore did not bring back the plaintext row, got %q", got)
	}
	if got := f.optionValue(t, "SMTPToken"); got != "smtp-plain" {
		t.Fatalf("restore did not bring back the plaintext option, got %q", got)
	}
}

func TestStatusReportsCounts(t *testing.T) {
	f := newFixture(t)
	if err := run(f.dsn, "status", "", "", "", false); err != nil {
		t.Fatalf("status: %v", err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var plain, encrypted int
	if err := db.QueryRow("SELECT COUNT(*) FROM channels WHERE `key` <> ''").Scan(&plain); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM channels WHERE `key` LIKE 'enc:v1:%'").Scan(&encrypted); err != nil {
		t.Fatalf("count: %v", err)
	}
	if plain != 2 || encrypted != 0 {
		t.Fatalf("fixture sanity check failed: plain=%d encrypted=%d", plain, encrypted)
	}
}
