package wado

import (
	"strings"
	"testing"
)

func TestValidateUID(t *testing.T) {
	valid := []string{
		"0", "1", "1.2", "1.2.840.10008.1.2.1", "1.2.3.4.5",
	}
	for _, uid := range valid {
		if err := ValidateUID(uid); err != nil {
			t.Errorf("ValidateUID(%q) = %v, want nil", uid, err)
		}
	}

	invalid := []string{
		"",                               // empty
		".",                              // empty components
		"1..2",                           // empty component
		"01.2",                           // leading zero
		"1.02",                           // leading zero in component
		"1.2a",                           // non-digit
		"../studies",                     // injection attempt
		"1.2%2e",                         // escaped dot
		"1.2." + strings.Repeat("9", 61), // exceeds 64 chars
	}
	for _, uid := range invalid {
		if err := ValidateUID(uid); err == nil {
			t.Errorf("ValidateUID(%q) = nil, want error", uid)
		}
	}
}
