package cli

import (
	"testing"

	"clawbridge/internal/faketool"
)

// TestMain lets a copy of this test binary act as a fake command-line tool.
func TestMain(m *testing.M) { faketool.Main(m) }
