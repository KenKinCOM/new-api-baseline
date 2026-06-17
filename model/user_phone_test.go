package model

import (
	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
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

func TestPhoneAndAlipayAlreadyTakenWhenMultipleRowsMatch(t *testing.T) {
	originalDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() {
		DB = originalDB
	})
	DB = db

	if err := DB.AutoMigrate(&User{}); err != nil {
		t.Fatalf("failed to migrate users: %v", err)
	}

	users := []User{
		{Username: "phone_duplicate_1", Phone: "13800138000", AlipayId: "2088000000000000", AffCode: "phone1"},
		{Username: "phone_duplicate_2", Phone: "13800138000", AlipayId: "2088000000000000", AffCode: "phone2"},
	}
	if err := DB.Create(&users).Error; err != nil {
		t.Fatalf("failed to create duplicate identity users: %v", err)
	}
	var duplicateCount int64
	if err := DB.Model(&User{}).Where("phone = ?", "13800138000").Count(&duplicateCount).Error; err != nil {
		t.Fatalf("failed to count duplicate phone users: %v", err)
	}
	if duplicateCount != 2 {
		t.Fatalf("expected duplicate phone fixture to have 2 rows, got %d", duplicateCount)
	}

	if err := DB.Callback().Query().After("gorm:query").Register("test:force_duplicate_rows_affected", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.RowsAffected = duplicateCount
		}
	}); err != nil {
		t.Fatalf("failed to register duplicate rows affected callback: %v", err)
	}

	if !IsPhoneAlreadyTaken("13800138000") {
		t.Fatalf("expected phone to be considered taken when multiple rows match")
	}
	if !IsAlipayIdAlreadyTaken("2088000000000000") {
		t.Fatalf("expected alipay id to be considered taken when multiple rows match")
	}
}
