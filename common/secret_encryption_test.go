package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTestSecretKey installs a fixed master key for the duration of the test and
// clears it afterwards, so tests never depend on the ambient environment.
func useTestSecretKey(t *testing.T, material string) {
	t.Helper()
	if err := SetSecretEncryptionKeyMaterial(material); err != nil {
		t.Fatalf("install test key: %v", err)
	}
	t.Cleanup(func() {
		ResetSecretEncryptionKeyForTest()
		_ = SetSecretEncryptionKeyMaterial("")
		ResetSecretEncryptionKeyForTest()
	})
}

func TestEncryptSecretRoundTrip(t *testing.T) {
	useTestSecretKey(t, "unit-test-master-key")

	const plaintext = "sk-upstream-abcdef0123456789"
	envelope, err := EncryptSecret(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(envelope, SecretCipherPrefix) {
		t.Fatalf("envelope %q does not carry the %q prefix", envelope, SecretCipherPrefix)
	}
	if strings.Contains(envelope, plaintext) {
		t.Fatal("envelope leaks the plaintext")
	}
	got, err := DecryptSecret(envelope)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
	}
	if !IsEncryptedSecret(envelope) || IsEncryptedSecret(plaintext) {
		t.Fatal("IsEncryptedSecret does not distinguish envelope from plaintext")
	}
}

func TestEncryptSecretUsesFreshNonce(t *testing.T) {
	useTestSecretKey(t, "unit-test-master-key")

	first, err := EncryptSecret("same-value")
	if err != nil {
		t.Fatalf("encrypt first: %v", err)
	}
	second, err := EncryptSecret("same-value")
	if err != nil {
		t.Fatalf("encrypt second: %v", err)
	}
	if first == second {
		t.Fatal("two encryptions of the same value produced identical envelopes (nonce reuse)")
	}
	for _, envelope := range []string{first, second} {
		if plaintext, err := DecryptSecret(envelope); err != nil || plaintext != "same-value" {
			t.Fatalf("decrypt %q: %q / %v", envelope, plaintext, err)
		}
	}
}

func TestEncryptSecretIsIdempotentOnEnvelope(t *testing.T) {
	useTestSecretKey(t, "unit-test-master-key")

	envelope, err := EncryptSecret("value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	again, err := EncryptSecret(envelope)
	if err != nil {
		t.Fatalf("re-encrypt: %v", err)
	}
	if again != envelope {
		t.Fatal("encrypting an existing envelope must not double-wrap it")
	}
}

func TestEncryptSecretWithoutKeyIsPlaintextNoop(t *testing.T) {
	useTestSecretKey(t, "")
	if SecretEncryptionEnabled() {
		t.Fatal("encryption must be disabled when no key is configured")
	}
	value, err := EncryptSecret("still-plaintext")
	if err != nil {
		t.Fatalf("encrypt without key: %v", err)
	}
	if value != "still-plaintext" {
		t.Fatalf("without a key the write path must stay plaintext, got %q", value)
	}
	if SecretEncryptionKeyOrigin() != SecretOriginUnset {
		t.Fatalf("expected unset origin, got %s", SecretEncryptionKeyOrigin())
	}
}

func TestDecryptSecretWithoutKeyFails(t *testing.T) {
	useTestSecretKey(t, "unit-test-master-key")
	envelope, err := EncryptSecret("value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	useTestSecretKey(t, "")

	if _, err := DecryptSecret(envelope); err == nil {
		t.Fatal("decrypting an envelope without a configured key must fail")
	} else if !strings.Contains(err.Error(), ErrSecretEncryptionKeyMissing.Error()) {
		t.Fatalf("expected the missing-key error, got %v", err)
	}
}

func TestDecryptSecretWithWrongKeyFails(t *testing.T) {
	useTestSecretKey(t, "key-number-one")
	envelope, err := EncryptSecret("value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	useTestSecretKey(t, "key-number-two")

	if _, err := DecryptSecret(envelope); err == nil {
		t.Fatal("a different master key must not decrypt the envelope")
	}
}

func TestDecryptSecretRejectsTamperedCiphertext(t *testing.T) {
	useTestSecretKey(t, "unit-test-master-key")
	envelope, err := EncryptSecret("value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	tampered := envelope[:len(envelope)-2] + "AA"

	if _, err := DecryptSecret(tampered); err == nil {
		t.Fatal("a tampered envelope must fail authentication")
	}
}

func TestDecryptSecretPassesLegacyPlaintextThrough(t *testing.T) {
	useTestSecretKey(t, "unit-test-master-key")
	value, err := DecryptSecret("legacy-plaintext-row")
	if err != nil {
		t.Fatalf("decrypt legacy: %v", err)
	}
	if value != "legacy-plaintext-row" {
		t.Fatalf("legacy plaintext must pass through, got %q", value)
	}
}

func TestSecretEncryptionKeyFromFile(t *testing.T) {
	useTestSecretKey(t, "")
	ResetSecretEncryptionKeyForTest()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret_encryption_key")
	if err := os.WriteFile(path, []byte("file-master-key\n"), 0o400); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv(SecretEncryptionKeyEnv, "")
	t.Setenv(SecretEncryptionKeyEnv+SecretFileSuffix, path)
	t.Cleanup(func() {
		ResetSecretEncryptionKeyForTest()
	})

	if !SecretEncryptionEnabled() {
		t.Fatal("expected encryption to be enabled from the key file")
	}
	if origin := SecretEncryptionKeyOrigin(); origin != SecretOriginFile {
		t.Fatalf("expected file origin, got %s", origin)
	}
	envelope, err := EncryptSecret("from-file-key")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if plaintext, err := DecryptSecret(envelope); err != nil || plaintext != "from-file-key" {
		t.Fatalf("round trip with file key: %q / %v", plaintext, err)
	}
}

func TestSecretEncryptionKeyFromEnv(t *testing.T) {
	useTestSecretKey(t, "")
	ResetSecretEncryptionKeyForTest()
	t.Setenv(SecretEncryptionKeyEnv+SecretFileSuffix, "")
	t.Setenv(SecretEncryptionKeyEnv, " env-master-key ")
	t.Cleanup(func() {
		ResetSecretEncryptionKeyForTest()
	})

	if !SecretEncryptionEnabled() {
		t.Fatal("expected encryption to be enabled from the environment")
	}
	if origin := SecretEncryptionKeyOrigin(); origin != SecretOriginEnv {
		t.Fatalf("expected env origin, got %s", origin)
	}
}
