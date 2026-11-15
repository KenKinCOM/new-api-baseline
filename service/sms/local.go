package sms

import (
	"context"
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

type LocalProvider struct {
	LogEnabled bool
}

func (p LocalProvider) Name() string {
	return "local"
}

func (p LocalProvider) SendVerificationCode(ctx context.Context, phone string, code string) error {
	if p.LogEnabled {
		common.SysLog(fmt.Sprintf("[SMS local development] phone=%s code=%s", MaskPhone(phone), code))
	}
	return nil
}
