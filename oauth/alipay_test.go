package oauth

import "testing"

func TestAlipayProviderMetadata(t *testing.T) {
	provider := &AlipayProvider{}

	if provider.GetName() != "Alipay" {
		t.Fatalf("expected Alipay provider name")
	}
	if provider.GetProviderPrefix() != "alipay_" {
		t.Fatalf("expected alipay username prefix")
	}
	if GetProvider("alipay") == nil {
		t.Fatalf("expected alipay provider to be registered")
	}
}
