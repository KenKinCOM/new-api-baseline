package common

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
)

// SecretFileSuffix is appended to a secret environment variable name to obtain
// the name of the variable that points at a file holding the secret value.
//
// Example: SESSION_SECRET_FILE=/run/secrets/session_secret makes the process
// read SESSION_SECRET from that file instead of from the process environment.
const SecretFileSuffix = "_FILE"

// SecretOrigin describes where a secret value came from. It never contains the
// secret itself and is safe to log.
type SecretOrigin string

const (
	SecretOriginUnset SecretOrigin = "unset"
	SecretOriginEnv   SecretOrigin = "env"
	SecretOriginFile  SecretOrigin = "file"
)

func (o SecretOrigin) String() string {
	return string(o)
}

// LoadSecret resolves a secret from either <name>_FILE (preferred, keeps the
// value out of the process environment / `ps e`, systemd unit files and
// container inspect output) or <name> itself.
//
// Resolution order:
//  1. <name>_FILE, when set and non-blank, is read from disk. Trailing
//     newlines/whitespace from editors and secret mounts are trimmed.
//  2. <name>, when set and non-blank.
//  3. Neither set: ("", SecretOriginUnset, nil) so callers can keep their own
//     defaults.
//
// Setting both is allowed for migration windows: the file wins and a warning is
// logged without printing either value.
func LoadSecret(name string) (value string, origin SecretOrigin, err error) {
	if name == "" {
		return "", SecretOriginUnset, errors.New("secret name must not be empty")
	}
	fileEnv := name + SecretFileSuffix
	filePath := strings.TrimSpace(os.Getenv(fileEnv))
	inline := strings.TrimSpace(os.Getenv(name))

	if filePath == "" {
		if inline == "" {
			return "", SecretOriginUnset, nil
		}
		return inline, SecretOriginEnv, nil
	}

	raw, readErr := os.ReadFile(filePath)
	if readErr != nil {
		return "", SecretOriginFile, fmt.Errorf("failed to read %s=%s: %w", fileEnv, filePath, readErr)
	}
	fileValue := strings.TrimSpace(string(raw))
	if fileValue == "" {
		return "", SecretOriginFile, fmt.Errorf("%s points at %s which is empty", fileEnv, filePath)
	}
	if inline != "" && inline != fileValue {
		log.Printf(LogText("WARNING: both %s and %s are set and differ; using the file value from %s"), name, fileEnv, filePath)
	}
	if info, statErr := os.Stat(filePath); statErr == nil && info.Mode().Perm()&0o044 != 0 {
		log.Printf(LogText("WARNING: secret file %s is readable by group/other (mode %s); restrict it to the service user (e.g. 0400)"), filePath, info.Mode().Perm().String())
	}
	return fileValue, SecretOriginFile, nil
}

// MustLoadSecret is LoadSecret with startup fatal semantics: any read failure
// aborts the process instead of silently falling back to a weaker value.
func MustLoadSecret(name string) (string, SecretOrigin) {
	value, origin, err := LoadSecret(name)
	if err != nil {
		log.Fatal(LogText("failed to load secret %s: %s", name, err.Error()))
	}
	return value, origin
}
