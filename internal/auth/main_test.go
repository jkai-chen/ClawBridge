package auth

import (
	"testing"

	"clawbridge/internal/faketool"
)

// TestMain lets a copy of this test binary act as a fake browser opener.
func TestMain(m *testing.M) { faketool.Main(m) }
