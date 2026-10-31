package backend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Setup error codes, shown by the UI as a setup panel.
const (
	CodeNoClient     = "no-client"     // no clawbridge found (macOS, Linux, native Windows)
	CodeNeedsDistro  = "needs-distro"  // Windows: no distro picked and no native client
	CodeBadDistro    = "bad-distro"    // the saved distro is gone
	CodeBadPath      = "bad-path"      // the saved client path is not usable
	CodeNoWSL        = "no-wsl"        // wsl.exe failed
	CodeSpawn        = "spawn-failed"  // the process would not start
	CodeExited       = "exited"        // the backend ended
	CodeFrame        = "frame"         // a bad or oversized frame
	CodeNoPreamble   = "no-preamble"   // no preamble line
	CodeNoHello      = "no-hello"      // hello failed or was not answered
	CodeUpdateNeeded = "update-needed" // protocol mismatch
)

// SetupError is a start failure the person can fix.
type SetupError struct {
	Code string
	Msg  string
}

func (e *SetupError) Error() string { return e.Msg }

// Plan is a resolved launch: what to run and how to describe it.
type Plan struct {
	Target string // "local", "native Windows", or "Ubuntu (WSL)"
	Distro string // set for WSL
	Cmd    *exec.Cmd
}

// Resolver turns the settings into a command. Its fields are injectable for
// tests; NewResolver fills them from the real machine.
type Resolver struct {
	GOOS     string
	Home     string
	Settings func() (Settings, error)
	LookPath func(string) (string, error)
	Distros  func() ([]string, error)
}

// NewResolver builds the real resolver over a settings store.
func NewResolver(store *SettingsStore) *Resolver {
	home, _ := os.UserHomeDir()
	return &Resolver{GOOS: runtime.GOOS, Home: home, Settings: store.Load, LookPath: exec.LookPath, Distros: ListWSLDistros}
}

// Resolve picks the launch for the current settings.
func (r *Resolver) Resolve() (*Plan, error) {
	set, err := r.Settings()
	if err != nil {
		return nil, &SetupError{CodeBadPath, err.Error()}
	}
	if r.GOOS == "windows" && set.WSLDistro != "" {
		list, err := r.Distros()
		if err != nil {
			return nil, &SetupError{CodeNoWSL, "Could not list WSL distros: " + err.Error()}
		}
		distro, err := CheckDistro(set.WSLDistro, list)
		if err != nil {
			return nil, &SetupError{CodeBadDistro, fmt.Sprintf("The saved WSL distro %q is not installed any more. Pick another.", set.WSLDistro)}
		}
		argv := WSLArgv(distro)
		cmd := exec.Command(argv[0], argv[1:]...)
		return &Plan{Target: distro + " (WSL)", Distro: distro, Cmd: cmd}, nil
	}
	path, err := r.findClient(set)
	if err != nil {
		var se *SetupError
		if r.GOOS == "windows" && errors.As(err, &se) && se.Code == CodeNoClient {
			return nil, &SetupError{CodeNeedsDistro, "Pick the WSL distro where ClawBridge is installed, or install the Windows client."}
		}
		return nil, err
	}
	cmd := exec.Command(path, "gui-backend", "--stdio")
	if r.Home != "" {
		cmd.Dir = r.Home
	}
	target := "this computer"
	if r.GOOS == "windows" {
		target = "Windows"
	}
	return &Plan{Target: target, Cmd: cmd}, nil
}

func (r *Resolver) findClient(set Settings) (string, error) {
	if set.BackendPath != "" {
		if err := checkExecutable(r.GOOS, set.BackendPath); err != nil {
			return "", &SetupError{CodeBadPath, fmt.Sprintf("The chosen client %q cannot be used: %v", set.BackendPath, err)}
		}
		return set.BackendPath, nil
	}
	if p, err := r.LookPath("clawbridge"); err == nil {
		return p, nil
	}
	// A GUI started from the Dock or Start menu has a short PATH that lacks
	// the installer's usual directories.
	if r.Home != "" {
		for _, dir := range []string{
			filepath.Join(r.Home, ".local", "bin"),
			filepath.Join(r.Home, "go", "bin"),
			"/usr/local/bin",
			"/opt/homebrew/bin",
		} {
			name := "clawbridge"
			if r.GOOS == "windows" {
				name += ".exe"
			}
			if p := filepath.Join(dir, name); checkExecutable(r.GOOS, p) == nil {
				return p, nil
			}
		}
	}
	return "", &SetupError{CodeNoClient, "No clawbridge client found on PATH. Install ClawBridge, or choose the client file."}
}

func checkExecutable(goos, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("the path is not absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a file")
	}
	if goos != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("not executable")
	}
	return nil
}

// Child is a started backend process.
type Child struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
	Wait   func() error
	Kill   func()
}

// StartCmd starts cmd with pipes on all three streams.
func StartCmd(cmd *exec.Cmd) (*Child, error) {
	hideWindow(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Child{
		Stdin: in, Stdout: out, Stderr: errp,
		Wait: cmd.Wait,
		Kill: func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		},
	}, nil
}
