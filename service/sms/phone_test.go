package sms

import "testing"

func TestNormalizePhone(t *testing.T) {
	got, err := NormalizePhone(" 138 0013 8000 ")
	if err != nil {
		t.Fatalf("NormalizePhone returned error: %v", err)
	}
	if got != "13800138000" {
		t.Fatalf("expected normalized phone, got %q", got)
	}
}

func TestNormalizePhoneRejectsInvalid(t *testing.T) {
	if _, err := NormalizePhone("abc"); err == nil {
		t.Fatalf("expected invalid phone error")
	}
}

func TestMaskPhone(t *testing.T) {
	got := MaskPhone("13800138000")
	if got != "138****8000" {
		t.Fatalf("expected masked phone, got %q", got)
	}
}
