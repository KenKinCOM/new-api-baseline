package sms

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/setting/system_setting"
)

type Provider interface {
	Name() string
	SendVerificationCode(ctx context.Context, phone string, code string) error
}

func GetProvider(settings *system_setting.SMSSettings) (Provider, error) {
	if settings == nil {
		return nil, errors.New("SMS settings are not configured")
	}
	switch settings.Provider {
	case "local", "":
		return LocalProvider{LogEnabled: settings.LocalLogEnabled}, nil
	default:
		return nil, errors.New("unsupported SMS provider")
	}
}
