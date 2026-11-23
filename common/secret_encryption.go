package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/hkdf"
)

// SecretEncryptionKeyEnv names the environment variable (or, with the _FILE
// suffix handled by LoadSecret, the file) that holds the master key material
// for at-rest secret encryption. The material never appears in source code or
// in the database: it is read from the process environment or from a file.
const SecretEncryptionKeyEnv = "SECRET_ENCRYPTION_KEY"

// SecretCipherPrefix marks a stored value as secret-encrypted. Values without
// the prefix are legacy plaintext and are returned unchanged, so a rollout can
// encrypt row by row and a rollback can stop halfway.
const SecretCipherPrefix = "enc:v1:"

// secretEncryptionInfo is the HKDF info string. It separates the data key
// derived here from every other key derived from the same material.
const secretEncryptionInfo = "new-api/secret-encryption/v1"

// ErrSecretEncryptionKeyMissing is returned when a ciphertext has to be
// decrypted but no master key is configured. It never carries the value.
var ErrSecretEncryptionKeyMissing = errors.New("secret encryption key is not configured: set " + SecretEncryptionKeyEnv + " or " + SecretEncryptionKeyEnv + SecretFileSuffix)

type secretCipherState struct {
	aead   cipher.AEAD
	origin SecretOrigin
}

var secretCipher struct {
	sync.RWMutex
	loaded bool
	state  *secretCipherState
	err    error
}

// SetSecretEncryptionKeyMaterial installs a master key explicitly. Tests use
// it; production code lets the environment decide through
// SecretEncryptionEnabled.
func SetSecretEncryptionKeyMaterial(material string) error {
	state, err := buildSecretCipher(material)
	secretCipher.Lock()
	defer secretCipher.Unlock()
	secretCipher.loaded = true
	secretCipher.state = state
	secretCipher.err = err
	return err
}

// ResetSecretEncryptionKeyForTest drops the cached key so the next use reloads
// it from the environment.
func ResetSecretEncryptionKeyForTest() {
	secretCipher.Lock()
	defer secretCipher.Unlock()
	secretCipher.loaded = false
	secretCipher.state = nil
	secretCipher.err = nil
}

func buildSecretCipher(material string) (*secretCipherState, error) {
	material = strings.TrimSpace(material)
	if material == "" {
		return nil, nil
	}
	// HKDF-SHA256 over the configured material yields the 32-byte AES-256 data
	// key, so the operator may use a passphrase of any length. The salt is
	// fixed: the data key must be reproducible across restarts and nodes.
	reader := hkdf.New(sha256.New, []byte(material), []byte("new-api"), []byte(secretEncryptionInfo))
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("derive secret encryption key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init secret cipher: %w", err)
	}
	return &secretCipherState{aead: aead}, nil
}

func loadSecretCipher() (*secretCipherState, error) {
	secretCipher.RLock()
	if secretCipher.loaded {
		state, err := secretCipher.state, secretCipher.err
		secretCipher.RUnlock()
		return state, err
	}
	secretCipher.RUnlock()

	secretCipher.Lock()
	defer secretCipher.Unlock()
	if secretCipher.loaded {
		return secretCipher.state, secretCipher.err
	}
	material, origin, err := LoadSecret(SecretEncryptionKeyEnv)
	if err != nil {
		secretCipher.loaded = true
		secretCipher.err = err
		return nil, err
	}
	state, err := buildSecretCipher(material)
	if err != nil {
		secretCipher.loaded = true
		secretCipher.err = err
		return nil, err
	}
	if state != nil {
		state.origin = origin
	}
	secretCipher.loaded = true
	secretCipher.state = state
	secretCipher.err = nil
	return state, nil
}

// SecretEncryptionEnabled reports whether a master key is configured. When it
// is false every write path stores plaintext exactly as before, which keeps
// existing deployments and tests unchanged until the operator opts in.
func SecretEncryptionEnabled() bool {
	state, err := loadSecretCipher()
	return err == nil && state != nil
}

// SecretEncryptionKeyOrigin reports where the master key came from ("env",
// "file" or "unset"). It never returns key material and is safe to log.
func SecretEncryptionKeyOrigin() SecretOrigin {
	state, _ := loadSecretCipher()
	if state == nil {
		return SecretOriginUnset
	}
	return state.origin
}

// IsEncryptedSecret reports whether value is a secret-encryption envelope.
func IsEncryptedSecret(value string) bool {
	return strings.HasPrefix(value, SecretCipherPrefix)
}

// EncryptSecret encrypts plaintext for storage. It is a no-op that returns
// plaintext unchanged when no master key is configured or when the value is
// already an envelope, so it is safe to call on every write path.
func EncryptSecret(plaintext string) (string, error) {
	if plaintext == "" || IsEncryptedSecret(plaintext) {
		return plaintext, nil
	}
	state, err := loadSecretCipher()
	if err != nil {
		return "", err
	}
	if state == nil {
		return plaintext, nil
	}
	nonce := make([]byte, state.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret encryption nonce: %w", err)
	}
	sealed := state.aead.Seal(nonce, nonce, []byte(plaintext), []byte(secretEncryptionInfo))
	return SecretCipherPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// DecryptSecret returns the plaintext of value. Legacy plaintext (no envelope
// prefix) is returned unchanged, which is what makes a row-by-row rollout and
// a partial rollback safe.
func DecryptSecret(value string) (string, error) {
	if !IsEncryptedSecret(value) {
		return value, nil
	}
	state, err := loadSecretCipher()
	if err != nil {
		return "", err
	}
	if state == nil {
		return "", ErrSecretEncryptionKeyMissing
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, SecretCipherPrefix))
	if err != nil {
		return "", errors.New("secret ciphertext is not valid base64")
	}
	nonceSize := state.aead.NonceSize()
	if len(raw) <= nonceSize {
		return "", errors.New("secret ciphertext is truncated")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := state.aead.Open(nil, nonce, ciphertext, []byte(secretEncryptionInfo))
	if err != nil {
		return "", errors.New("secret ciphertext failed authentication (wrong key or corrupted value)")
	}
	return string(plaintext), nil
}
