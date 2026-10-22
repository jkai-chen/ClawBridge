package cli

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestWizardLinesEndAtLFCROrCRLF(t *testing.T) {
	for input, want := range map[string][]string{
		"server\r\nname\r\n": {"server", "name"}, // Windows console
		"server\rname\r":     {"server", "name"}, // a raw terminal's Enter
		"server\nname\n":     {"server", "name"},
		"a\n\nb\n":           {"a", "", "b"}, // an empty answer stays empty
		"\r\n\r\nx\n":        {"", "", "x"},
	} {
		wizardAfterCR.Store(false)
		r := strings.NewReader(input)
		for i, line := range want {
			got, err := readWizardLine(r)
			if err != nil || got != line {
				t.Fatalf("%q line %d: got %q, %v; want %q", input, i, got, err, line)
			}
		}
	}
	wizardAfterCR.Store(false)
}

// On Windows the console must not reach cancelreader as an *os.File, whose
// Windows reader switches the console to raw mode.
func TestWizardInputKeepsTheWindowsConsoleCooked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix pipe to tell the cancellable reader from the fallback")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	windows, err := newWizardInput("windows", r)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.Close()
	if windows.Cancel() {
		t.Fatal("Windows wizard input used the console-owning cancellable reader")
	}
	native, err := newWizardInput(runtime.GOOS, r)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if !native.Cancel() {
		t.Fatal("non-Windows wizard input lost cancellation")
	}
}
