package broken

import "strings"

// Broken declares an undefined identifier so indexing records a type error.
func Broken() string {
	return strings.ToUpper(speel) + undefinedHelper()
}
