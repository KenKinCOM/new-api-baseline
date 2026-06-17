package model

import (
	"reflect"
	"strings"
	"testing"
)

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

func TestUserPhoneFieldUsesNonUniqueIndex(t *testing.T) {
	field, ok := reflect.TypeOf(User{}).FieldByName("Phone")
	if !ok {
		t.Fatalf("expected phone field to be available")
	}

	gormTag := field.Tag.Get("gorm")
	if !strings.Contains(gormTag, "index") {
		t.Fatalf("expected phone field to be indexed")
	}
	if strings.Contains(gormTag, "uniqueIndex") {
		t.Fatalf("expected phone field not to use uniqueIndex")
	}
}

func TestEmptyPhoneIsNotAlreadyTaken(t *testing.T) {
	if IsPhoneAlreadyTaken("") {
		t.Fatalf("expected empty phone not to be considered taken")
	}
}
