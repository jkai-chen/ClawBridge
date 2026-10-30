//go:build windows

package integration

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The guided setup reads its answers from the Windows console a person types
// into. A pseudo console (ConPTY) gives the real CLI that console, so this
// covers echo, Backspace, Enter and Ctrl+C instead of a stubbed reader.
func TestWindowsGuidedSetupConsoleInput(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "clawbridge.exe")
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/clawbridge")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	config := filepath.Join(dir, "client.json")
	env := []string{"CLAWBRIDGE_HOME=" + filepath.Join(dir, "state")}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "CLAWBRIDGE_") {
			env = append(env, e)
		}
	}
	c := startConsole(t, []string{binary, "--client-config", config}, env)

	// Start comes first; its "2" leads to the guided join.
	c.expect("Choose 1, 2 or 3:", "Start did not prompt in a console")
	c.send("2\r")
	c.expect("Service HTTPS URL:", "the guided setup did not prompt in a console")
	c.send("https://example.invalid")
	c.expect("https://example.invalid", "typed text was not echoed")
	c.send("\r")
	// The service can't be reached, so the invite is tried anyway: the only
	// way in since v1.0 removed Google sign-in.
	c.expect("trying an invite anyway", "Enter did not complete the prompt")
	c.expect("Email the invite was issued for", "the invite path did not ask for the email")
	// "adq", Backspace, "a@example.test" must answer "ada@example.test"; a
	// Backspace stored as a byte would fail the email check and ask again.
	c.send("adq\x7fa@example.test\r")
	c.expect("Name for this device", "Enter did not complete the email prompt")
	// "laptoq", Backspace, "p" must answer "laptop".
	c.send("laptoq\x7fp\r")
	// The wizard's own summary shows the answers it received.
	c.expect("Email: ada@example.test", "Backspace did not edit the email answer")
	c.expect("This device: laptop", "Backspace did not edit the device name")
	c.expect("Continue, then paste the invite link or code?", "the confirmation prompt did not appear")
	c.send("\x03")
	if code := c.wait(15 * time.Second); code == 0 {
		t.Fatal("Ctrl+C at the confirmation prompt did not cancel the guided setup")
	}
	if strings.Contains(c.text(), squash("Invite link or code")) {
		t.Fatal("Ctrl+C was ignored: the guided setup went on to ask for the code")
	}
	if _, err := os.Lstat(config); !os.IsNotExist(err) {
		t.Fatal("a canceled guided setup wrote the client config", err)
	}
}

type consoleProcess struct {
	t       *testing.T
	process windows.Handle
	input   *os.File
	mu      sync.Mutex
	raw     bytes.Buffer
	pos     int
}

func startConsole(t *testing.T, argv, env []string) *consoleProcess {
	t.Helper()
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		t.Fatal(err)
	}
	var console windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: 160, Y: 50}, inRead, outWrite, 0, &console); err != nil {
		t.Fatalf("CreatePseudoConsole: %v", err)
	}
	// The pseudo console holds its own duplicates of these pipe ends.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	// UpdateProcThreadAttribute takes the pseudo console handle itself as the
	// attribute value, not a pointer to it.
	update := windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")
	if r, _, e := update.Call(uintptr(unsafe.Pointer(attrs.List())), 0, windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0); r == 0 {
		t.Fatalf("UpdateProcThreadAttribute: %v", e)
	}
	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Without STARTF_USESTDHANDLES the child inherits this test's redirected
	// handles instead of attaching to the pseudo console.
	si.Flags = windows.STARTF_USESTDHANDLES
	si.ProcThreadAttributeList = attrs.List()
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		t.Fatal(err)
	}
	block := []uint16{}
	for _, e := range env {
		block = append(block, utf16.Encode([]rune(e))...)
		block = append(block, 0)
	}
	block = append(block, 0)
	pi := new(windows.ProcessInformation)
	if err = windows.CreateProcess(nil, cmdline, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, &block[0], nil, &si.StartupInfo, pi); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	windows.CloseHandle(pi.Thread)
	c := &consoleProcess{t: t, process: pi.Process, input: os.NewFile(uintptr(inWrite), "conpty-input")}
	output := os.NewFile(uintptr(outRead), "conpty-output")
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 4096)
		for {
			n, err := output.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.raw.Write(buf[:n])
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		windows.TerminateProcess(c.process, 1)
		c.input.Close()
		// Closing the pseudo console ends the output pipe; keep draining it
		// meanwhile, or the close can block.
		windows.ClosePseudoConsole(console)
		select {
		case <-drained:
		case <-time.After(10 * time.Second):
			t.Error("pseudo console output did not close")
		}
		output.Close()
		windows.CloseHandle(c.process)
		if t.Failed() {
			c.mu.Lock()
			t.Logf("console output: %q", c.raw.String())
			c.mu.Unlock()
		}
	})
	return c
}

func (c *consoleProcess) send(keys string) {
	c.t.Helper()
	if _, err := c.input.Write([]byte(keys)); err != nil {
		c.t.Fatalf("write console input: %v", err)
	}
}

// expect waits for text after the previous match. Console output is compared
// without escape sequences or whitespace, which the pseudo console may render
// as cursor movement.
func (c *consoleProcess) expect(text, failure string) {
	c.t.Helper()
	want := squash(text)
	deadline := time.Now().Add(30 * time.Second)
	for {
		seen := c.text()
		if c.pos <= len(seen) {
			if at := strings.Index(seen[c.pos:], want); at >= 0 {
				c.pos += at + len(want)
				return
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: %q not seen after %q", failure, text, seen[min(c.pos, len(seen)):])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *consoleProcess) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return squash(consoleEscapes.ReplaceAllString(c.raw.String(), ""))
}

func (c *consoleProcess) wait(timeout time.Duration) uint32 {
	c.t.Helper()
	if event, err := windows.WaitForSingleObject(c.process, uint32(timeout.Milliseconds())); err != nil || event != windows.WAIT_OBJECT_0 {
		c.t.Fatalf("console process did not exit: %v", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.process, &code); err != nil {
		c.t.Fatal(err)
	}
	return code
}

var consoleEscapes = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
