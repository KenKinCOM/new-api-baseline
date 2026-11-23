package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// useSecretEncryptionTestDB gives the test its own in-memory database and a
// fixed master key, so it never touches the operator's real key material.
func useSecretEncryptionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	if err := common.SetSecretEncryptionKeyMaterial("model-test-master-key"); err != nil {
		t.Fatalf("install test key: %v", err)
	}
	t.Cleanup(func() {
		common.ResetSecretEncryptionKeyForTest()
	})

	driver := sqlite.Open(fmt.Sprintf("file:secret_%d?mode=memory&cache=shared", time.Now().UnixNano()))
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("secret_%d_", time.Now().UnixNano())}})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	tables := []any{&Channel{}, &Option{}, &SystemTask{}}
	if err := db.AutoMigrate(tables...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	// The shared in-memory database disappears when its last connection closes.
	sqlDB.SetMaxOpenConns(2)
	t.Cleanup(func() {
		requireNoError(t, db.Migrator().DropTable(tables...))
		requireNoError(t, sqlDB.Close())
	})
	previous := DB
	DB = db
	t.Cleanup(func() { DB = previous })
	return db
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChannelKeyIsEncryptedAtRest(t *testing.T) {
	db := useSecretEncryptionTestDB(t)

	const plaintext = "sk-upstream-plaintext"
	channel := Channel{Name: "encrypted", Key: plaintext, Status: common.ChannelStatusEnabled}
	requireNoError(t, db.Create(&channel).Error)
	if channel.Key != plaintext {
		t.Fatalf("AfterSave must restore plaintext in memory, got %q", channel.Key)
	}

	var stored string
	requireNoError(t, db.Raw(fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", "`key`", db.NamingStrategy.TableName("channels")), channel.Id).Scan(&stored).Error)
	if !common.IsEncryptedSecret(stored) {
		t.Fatalf("stored channel key is not an envelope: %q", stored)
	}
	if stored == plaintext {
		t.Fatal("stored channel key equals the plaintext")
	}

	var reloaded Channel
	requireNoError(t, db.First(&reloaded, channel.Id).Error)
	if reloaded.Key != plaintext {
		t.Fatalf("AfterFind must return plaintext, got %q", reloaded.Key)
	}
}

func TestChannelKeyLegacyPlaintextStillReadable(t *testing.T) {
	db := useSecretEncryptionTestDB(t)

	// Simulate a row written before the migration ran.
	requireNoError(t, db.Exec(fmt.Sprintf("INSERT INTO %s (name, %s, status) VALUES (?, ?, ?)",
		db.NamingStrategy.TableName("channels"), "`key`"), "legacy", "legacy-plaintext-key", common.ChannelStatusEnabled).Error)

	var reloaded Channel
	requireNoError(t, db.Where("name = ?", "legacy").First(&reloaded).Error)
	if reloaded.Key != "legacy-plaintext-key" {
		t.Fatalf("legacy plaintext row must pass through unchanged, got %q", reloaded.Key)
	}
}

func TestChannelKeyStaysPlaintextWithoutKey(t *testing.T) {
	db := useSecretEncryptionTestDB(t)
	requireNoError(t, common.SetSecretEncryptionKeyMaterial(""))
	if common.SecretEncryptionEnabled() {
		t.Fatal("encryption should be disabled without key material")
	}

	channel := Channel{Name: "plain", Key: "sk-plain", Status: common.ChannelStatusEnabled}
	requireNoError(t, db.Create(&channel).Error)

	var stored string
	requireNoError(t, db.Raw(fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", "`key`", db.NamingStrategy.TableName("channels")), channel.Id).Scan(&stored).Error)
	if stored != "sk-plain" {
		t.Fatalf("without a master key the column must stay plaintext, got %q", stored)
	}
}

func TestSensitiveOptionRoundTrip(t *testing.T) {
	useSecretEncryptionTestDB(t)

	stored, err := encryptOptionValueForStorage("SMTPToken", "smtp-secret")
	if err != nil {
		t.Fatalf("encrypt option: %v", err)
	}
	if !common.IsEncryptedSecret(stored) {
		t.Fatalf("sensitive option value was not encrypted: %q", stored)
	}
	plaintext, err := decryptOptionValueForRead("SMTPToken", stored)
	if err != nil {
		t.Fatalf("decrypt option: %v", err)
	}
	if plaintext != "smtp-secret" {
		t.Fatalf("round trip mismatch: %q", plaintext)
	}

	untouched, err := encryptOptionValueForStorage("Logo", "logo.png")
	if err != nil {
		t.Fatalf("non-sensitive option: %v", err)
	}
	if untouched != "logo.png" {
		t.Fatalf("non-sensitive option must stay plaintext, got %q", untouched)
	}
}

func TestUpdateOptionEncryptsSensitiveValue(t *testing.T) {
	useSecretEncryptionTestDB(t)
	InitOptionMap()
	t.Cleanup(InitOptionMap)

	if err := UpdateOption("SMTPToken", "smtp-live-secret"); err != nil {
		t.Fatalf("update sensitive option: %v", err)
	}
	if err := UpdateOption("Logo", "brand.png"); err != nil {
		t.Fatalf("update plain option: %v", err)
	}

	rows, err := AllOption()
	if err != nil {
		t.Fatalf("all options: %v", err)
	}
	seen := map[string]string{}
	for _, option := range rows {
		seen[option.Key] = option.Value
	}
	if !common.IsEncryptedSecret(seen["SMTPToken"]) {
		t.Fatalf("SMTPToken was stored as %q, want an envelope", seen["SMTPToken"])
	}
	if seen["Logo"] != "brand.png" {
		t.Fatalf("Logo must stay plaintext, got %q", seen["Logo"])
	}

	if err := LoadOptionsFromDatabase(); err != nil {
		t.Fatalf("load options: %v", err)
	}
	common.OptionMapRWMutex.RLock()
	inMemory := common.OptionMap["SMTPToken"]
	common.OptionMapRWMutex.RUnlock()
	if inMemory != "smtp-live-secret" {
		t.Fatalf("in-memory option must hold plaintext, got %q", inMemory)
	}
}
