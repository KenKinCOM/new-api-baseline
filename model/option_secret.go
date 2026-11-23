package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

// sensitiveOptionKeys lists the options whose value is a credential. They are
// encrypted at rest with the same envelope as Channel.Key; every other option
// stays plaintext so operators can keep reading the table.
//
// Keep this list in sync with the compliance inventory (T4 §2.2): 17 flat keys
// plus 4 registered-configuration keys flattened as "module.field".
var sensitiveOptionKeys = map[string]struct{}{
	"ResendApiKey":           {},
	"SMTPToken":              {},
	"WorkerValidKey":         {},
	"EpayKey":                {},
	"StripeApiSecret":        {},
	"StripeWebhookSecret":    {},
	"CreemApiKey":            {},
	"CreemWebhookSecret":     {},
	"WaffoApiKey":            {},
	"WaffoPrivateKey":        {},
	"WaffoSandboxApiKey":     {},
	"WaffoSandboxPrivateKey": {},
	"WaffoPancakePrivateKey": {},
	"GitHubClientSecret":     {},
	"TelegramBotToken":       {},
	"WeChatServerToken":      {},
	"TurnstileSecretKey":     {},
	"alipay.app_private_key": {},
	"oidc.client_secret":     {},
	"discord.client_secret":  {},
	"telegram.client_secret": {},
}

// IsSensitiveOption reports whether an option value must be encrypted at rest.
func IsSensitiveOption(key string) bool {
	_, ok := sensitiveOptionKeys[key]
	return ok
}

// SensitiveOptionKeys returns the registered keys as a slice, for the one-time
// migration and for tests.
func SensitiveOptionKeys() []string {
	keys := make([]string, 0, len(sensitiveOptionKeys))
	for key := range sensitiveOptionKeys {
		keys = append(keys, key)
	}
	return keys
}

// encryptOptionValueForStorage returns the value to store for key: an envelope
// for credentials, the untouched value for everything else.
func encryptOptionValueForStorage(key string, value string) (string, error) {
	if !IsSensitiveOption(key) {
		return value, nil
	}
	encrypted, err := common.EncryptSecret(value)
	if err != nil {
		return "", fmt.Errorf("encrypt option %s: %w", key, err)
	}
	return encrypted, nil
}

// decryptOptionValueForRead returns the plaintext to hold in memory. Legacy
// plaintext rows pass through untouched.
func decryptOptionValueForRead(key string, value string) (string, error) {
	if !IsSensitiveOption(key) {
		return value, nil
	}
	plaintext, err := common.DecryptSecret(value)
	if err != nil {
		return "", fmt.Errorf("decrypt option %s: %w", key, err)
	}
	return plaintext, nil
}
