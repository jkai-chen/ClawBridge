package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"

	"clawbridge/gui/backend"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Shell is the one object bound into the webview. Wails exposes every
// exported method, so this list is the page's entire reach into the Go
// side. In particular there is no method that takes a path or a command:
// the client path comes from a native file dialog the person answers, the
// WSL distro only from the list `wsl.exe -l -q` prints, and every backend
// call goes through backend.Bridge.Call's allow-list.
type Shell struct {
	app      *application.App
	bridge   *backend.Bridge
	settings *backend.SettingsStore

	mu         sync.Mutex
	violations []string
	ready      chan bool // the page's first-render signal; buffered, sent once
}

// maxViolations bounds what the shell keeps from a misbehaving page.
const maxViolations = 100

// ReportViolation is the page telling the shell that the webview blocked
// something: a Content-Security-Policy or Trusted Types violation. The shell
// logs and counts each one; the self-check fails on any.
func (s *Shell) ReportViolation(report string) {
	if len(report) > 2048 {
		report = report[:2048]
	}
	report = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, report)
	s.mu.Lock()
	if len(s.violations) < maxViolations {
		s.violations = append(s.violations, report)
	}
	s.mu.Unlock()
	log.Printf("webview violation: %s", report)
}

// violationList returns what was reported so far.
func (s *Shell) violationList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// Ready is the page saying its shell has rendered: the sidebar is in the
// DOM. rendered is false if it was not.
func (s *Shell) Ready(rendered bool) {
	select {
	case s.ready <- rendered:
	default:
	}
}

// Call forwards one allow-listed read to the backend. It refuses any params
// object that carries a nonce field (GUI-DESIGN §4.2).
func (s *Shell) Call(method string, params json.RawMessage) (json.RawMessage, error) {
	return s.bridge.Call(method, params)
}

// Status is the backend connection's current state.
func (s *Shell) Status() backend.Status { return s.bridge.Status() }

// Restart stops the backend and starts it again. Nothing that was in flight
// is replayed.
func (s *Shell) Restart() { s.bridge.Start() }

// Logs returns the backend's discarded output and stderr for Help → Logs.
func (s *Shell) Logs() []string { return s.bridge.Logs() }

// WSLDistros lists the WSL distros on Windows, and nothing elsewhere.
func (s *Shell) WSLDistros() ([]string, error) {
	if runtime.GOOS != "windows" {
		return []string{}, nil
	}
	return backend.ListWSLDistros()
}

// SetWSLDistro stores the distro to run the backend in, then restarts it.
// The name must match the live `wsl.exe -l -q` list exactly. An empty name
// clears the choice (the native Windows client is used if there is one).
func (s *Shell) SetWSLDistro(name string) error {
	if runtime.GOOS != "windows" {
		return errors.New("WSL is a Windows setting")
	}
	set, err := s.settings.Load()
	if err != nil {
		return err
	}
	if name != "" {
		list, err := backend.ListWSLDistros()
		if err != nil {
			return err
		}
		if name, err = backend.CheckDistro(name, list); err != nil {
			return err
		}
	}
	set.WSLDistro = name
	if err := s.settings.Save(set); err != nil {
		return err
	}
	s.bridge.Start()
	return nil
}

// ChooseClient opens the native file dialog so the person can point the GUI
// at a clawbridge file, stores the answer and restarts the backend. The page
// never supplies the path.
func (s *Shell) ChooseClient() (string, error) {
	path, err := s.app.Dialog.OpenFile().
		SetTitle("Choose the clawbridge client").
		CanChooseFiles(true).
		CanChooseDirectories(false).
		PromptForSingleSelection()
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil // cancelled
	}
	set, err := s.settings.Load()
	if err != nil {
		return "", err
	}
	set.BackendPath = path
	if err := s.settings.Save(set); err != nil {
		return "", fmt.Errorf("could not save the choice: %w", err)
	}
	s.bridge.Start()
	return path, nil
}
