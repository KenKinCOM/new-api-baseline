package common

import (
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type verificationValue struct {
	code         string
	time         time.Time
	validMinutes int
}

const (
	EmailVerificationPurpose = "v"
	PasswordResetPurpose     = "r"
	PhoneVerificationPurpose = "p"
)

var verificationMutex sync.Mutex
var verificationMap map[string]verificationValue
var verificationMapMaxSize = 10
var VerificationValidMinutes = 10

func GenerateVerificationCode(length int) string {
	code := uuid.New().String()
	code = strings.Replace(code, "-", "", -1)
	if length == 0 {
		return code
	}
	return code[:length]
}

func RegisterVerificationCodeWithKey(key string, code string, purpose string) {
	RegisterVerificationCodeWithKeyMinutes(key, code, purpose, VerificationValidMinutes)
}

func RegisterVerificationCodeWithKeyMinutes(key string, code string, purpose string, validMinutes int) {
	if validMinutes <= 0 {
		validMinutes = VerificationValidMinutes
	}
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	verificationMap[purpose+key] = verificationValue{
		code:         code,
		time:         time.Now(),
		validMinutes: validMinutes,
	}
	if len(verificationMap) > verificationMapMaxSize {
		removeExpiredPairs()
	}
}

func VerifyCodeWithKey(key string, code string, purpose string) bool {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	value, okay := verificationMap[purpose+key]
	now := time.Now()
	validMinutes := value.validMinutes
	if validMinutes <= 0 {
		validMinutes = VerificationValidMinutes
	}
	if !okay || int(now.Sub(value.time).Seconds()) >= validMinutes*60 {
		return false
	}
	return code == value.code
}

func DeleteKey(key string, purpose string) {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	delete(verificationMap, purpose+key)
}

// no lock inside, so the caller must lock the verificationMap before calling!
func removeExpiredPairs() {
	now := time.Now()
	for key := range verificationMap {
		validMinutes := verificationMap[key].validMinutes
		if validMinutes <= 0 {
			validMinutes = VerificationValidMinutes
		}
		if int(now.Sub(verificationMap[key].time).Seconds()) >= validMinutes*60 {
			delete(verificationMap, key)
		}
	}
}

func init() {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	verificationMap = make(map[string]verificationValue)
}
