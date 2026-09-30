package wado

import (
	"errors"
	"fmt"
)

// ValidateUID validates a UID against the DICOM whitelist rules: only the
// characters [0-9.], total length <= 64, non-empty components, and no leading
// zeros within a component (except a standalone "0").
//
// It exists to (1) prevent injection when the UID becomes a URL path segment
// and (2) turn a server-side 400 into a clear client-side error.
func ValidateUID(uid string) error {
	if uid == "" {
		return errors.New("empty UID")
	}
	if len(uid) > 64 {
		return fmt.Errorf("length %d exceeds 64", len(uid))
	}
	start := 0
	for i := 0; i <= len(uid); i++ {
		if i < len(uid) && uid[i] != '.' {
			continue
		}
		comp := uid[start:i]
		if comp == "" {
			return errors.New("empty component")
		}
		if len(comp) > 1 && comp[0] == '0' {
			return fmt.Errorf("component %q has leading zero", comp)
		}
		for j := 0; j < len(comp); j++ {
			if comp[j] < '0' || comp[j] > '9' {
				return fmt.Errorf("component %q contains non-digit", comp)
			}
		}
		start = i + 1
	}
	return nil
}
