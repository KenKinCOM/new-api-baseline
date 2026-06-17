package model

import "testing"

func TestUserHasPhoneAndAlipayFields(t *testing.T) {
	user := User{
		Username: "phone_1",
		Phone:    "13800138000",
		AlipayId: "2088000000000000",
	}

	if user.Phone != "13800138000" {
		t.Fatalf("expected phone field to be available")
	}
	if user.AlipayId != "2088000000000000" {
		t.Fatalf("expected alipay id field to be available")
	}
}
