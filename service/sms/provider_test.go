package sms

import (
	"context"
	"testing"
)

func TestLocalProviderSendsWithoutExternalService(t *testing.T) {
	provider := LocalProvider{LogEnabled: true}
	if provider.Name() != "local" {
		t.Fatalf("expected local provider")
	}
	if err := provider.SendVerificationCode(context.Background(), "13800138000", "123456"); err != nil {
		t.Fatalf("expected local provider success, got %v", err)
	}
}
