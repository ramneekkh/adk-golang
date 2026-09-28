package killswitch

import (
	"os"
)

// Disabled checks if a Functional Requirement (FR) mechanism is disabled via kill-switch.
func Disabled(fr string) bool {
	// "VA_DISABLE_FR_<ID>=1"
	key := "VA_DISABLE_FR_" + fr
	return os.Getenv(key) == "1"
}
