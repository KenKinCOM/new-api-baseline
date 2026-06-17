package sms

import (
	"errors"
	"regexp"
	"strings"
)

var phonePattern = regexp.MustCompile(`^\+?[0-9]{6,20}$`)

func NormalizePhone(phone string) (string, error) {
	normalized := strings.TrimSpace(phone)
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "-", "")
	if !phonePattern.MatchString(normalized) {
		return "", errors.New("invalid phone number")
	}
	return normalized, nil
}

func MaskPhone(phone string) string {
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}
