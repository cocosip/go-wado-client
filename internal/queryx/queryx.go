// Package queryx holds query-string helpers shared by the service packages
// (wadors / qido / wadouri).
package queryx

import "strconv"

// FormatFloat renders a float without exponent notation — some servers fail
// to parse values like "1e+07".
func FormatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
