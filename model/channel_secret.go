package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// Channel.Key holds upstream provider credentials and is the only channel
// column that is secret-encrypted at rest.
//
// The hooks below keep the encryption at the model boundary, so every caller —
// console handlers, CSV import, batch jobs, plugins — keeps working with the
// plaintext key in Go memory while the database only ever holds an
// `enc:v1:` envelope. Encryption is opt-in: when no SECRET_ENCRYPTION_KEY is
// configured, EncryptSecret returns the value unchanged and this is a no-op.
//
// Writes that go through a map instead of the struct (Updates(map[string]any))
// bypass these hooks, so those three call sites encrypt explicitly; see
// channelKeyForWrite.

// BeforeSave encrypts the key on its way to the database. AfterSave restores
// the in-memory value so a caller that keeps using the struct still reads
// plaintext.
func (channel *Channel) BeforeSave(_ *gorm.DB) error {
	encrypted, err := common.EncryptSecret(channel.Key)
	if err != nil {
		return fmt.Errorf("encrypt channel %d key: %w", channel.Id, err)
	}
	channel.Key = encrypted
	return nil
}

func (channel *Channel) AfterSave(_ *gorm.DB) error {
	plaintext, err := common.DecryptSecret(channel.Key)
	if err != nil {
		return fmt.Errorf("decrypt channel %d key after save: %w", channel.Id, err)
	}
	channel.Key = plaintext
	return nil
}

// AfterFind decrypts rows read from the database. Legacy plaintext rows pass
// through untouched, so the service runs while the one-time migration is still
// in progress.
func (channel *Channel) AfterFind(_ *gorm.DB) error {
	plaintext, err := common.DecryptSecret(channel.Key)
	if err != nil {
		return fmt.Errorf("decrypt channel %d key: %w", channel.Id, err)
	}
	channel.Key = plaintext
	return nil
}

// channelKeyForWrite converts a plaintext key into the value to store when the
// write path is a column map rather than a struct save.
func channelKeyForWrite(key string) (string, error) {
	encrypted, err := common.EncryptSecret(key)
	if err != nil {
		return "", fmt.Errorf("encrypt channel key: %w", err)
	}
	return encrypted, nil
}

// channelKeyMatchValues returns the stored representations a compare-and-swap
// guard has to accept: the encrypted form, plus the legacy plaintext form for
// rows the migration has not rewritten yet.
func channelKeyMatchValues(plaintext string) ([]string, error) {
	encrypted, err := channelKeyForWrite(plaintext)
	if err != nil {
		return nil, err
	}
	if encrypted == plaintext {
		return []string{plaintext}, nil
	}
	return []string{encrypted, plaintext}, nil
}
