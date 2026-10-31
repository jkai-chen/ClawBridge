package backend

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func newTestResolver(goos string, set Settings, distros []string, onPath string) *Resolver {
	return &Resolver{
		GOOS: goos, Home: "/nonexistent-home",
		Settings: func() (Settings, error) { return set, nil },
		LookPath: func(string) (string, error) {
			if onPath == "" {
				return "", errors.New("not found")
			}
			return onPath, nil
		},
		Distros: func() ([]string, error) { return distros, nil },
	}
}

func setupCode(t *testing.T, err error) string {
	t.Helper()
	var se *SetupError
	if !errors.As(err, &se) {
		t.Fatalf("not a SetupError: %v", err)
	}
	return se.Code
}

func TestResolveLocalUsesPATH(t *testing.T) {
	p, err := newTestResolver("linux", Settings{}, nil, "/usr/bin/clawbridge").Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Cmd.Args, []string{"/usr/bin/clawbridge", "gui-backend", "--stdio"}) {
		t.Fatalf("argv %q", p.Cmd.Args)
	}
}

func TestResolveLocalUsesTheSettingsPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "clawbridge")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newTestResolver("darwin", Settings{BackendPath: bin}, nil, "/usr/bin/other").Resolve()
	if err != nil || p.Cmd.Path != bin {
		t.Fatalf("%v %v", p, err)
	}
	// Not absolute, missing, not executable, a directory: all refused.
	notExec := filepath.Join(t.TempDir(), "plain")
	_ = os.WriteFile(notExec, []byte("x"), 0o644)
	for _, bad := range []string{"clawbridge", "./clawbridge", "/no/such/file", notExec, t.TempDir()} {
		_, err := newTestResolver("darwin", Settings{BackendPath: bad}, nil, "/usr/bin/other").Resolve()
		if setupCode(t, err) != CodeBadPath {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestResolveNoClient(t *testing.T) {
	_, err := newTestResolver("linux", Settings{}, nil, "").Resolve()
	if setupCode(t, err) != CodeNoClient {
		t.Fatalf("%v", err)
	}
}

func TestResolveWindowsWithWSL(t *testing.T) {
	r := newTestResolver("windows", Settings{WSLDistro: "Ubuntu-24.04"}, []string{"Debian", "Ubuntu-24.04"}, "")
	p, err := r.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Cmd.Args, WSLArgv("Ubuntu-24.04")) || p.Distro != "Ubuntu-24.04" || p.Target != "Ubuntu-24.04 (WSL)" {
		t.Fatalf("plan %+v argv %q", p, p.Cmd.Args)
	}
}

func TestResolveWindowsRechecksTheDistro(t *testing.T) {
	// The saved name must still match `wsl.exe -l -q` exactly at launch.
	r := newTestResolver("windows", Settings{WSLDistro: "Ubuntu"}, []string{"Debian"}, "")
	if _, err := r.Resolve(); setupCode(t, err) != CodeBadDistro {
		t.Fatalf("%v", err)
	}
	r = newTestResolver("windows", Settings{WSLDistro: "--exec"}, []string{"--exec"}, "")
	if _, err := r.Resolve(); setupCode(t, err) != CodeBadDistro {
		t.Fatalf("%v", err)
	}
	r = newTestResolver("windows", Settings{WSLDistro: "Debian"}, nil, "")
	r.Distros = func() ([]string, error) { return nil, errors.New("wsl.exe not found") }
	if _, err := r.Resolve(); setupCode(t, err) != CodeNoWSL {
		t.Fatalf("%v", err)
	}
}

func TestResolveWindowsNeedsAPickWhenNothingIsSet(t *testing.T) {
	if _, err := newTestResolver("windows", Settings{}, []string{"Debian"}, "").Resolve(); setupCode(t, err) != CodeNeedsDistro {
		t.Fatalf("%v", err)
	}
	// A native client on PATH is offered without a distro.
	p, err := newTestResolver("windows", Settings{}, nil, `C:\bin\clawbridge.exe`).Resolve()
	if err != nil || p.Target != "Windows" || p.Cmd.Args[1] != "gui-backend" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := &SettingsStore{Path: filepath.Join(t.TempDir(), "sub", "settings.json")}
	if got, err := s.Load(); err != nil || got != (Settings{}) {
		t.Fatalf("missing file: %+v %v", got, err)
	}
	want := Settings{BackendPath: "/x/clawbridge", WSLDistro: "Ubuntu"}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(); err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
	if info, _ := os.Stat(s.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	_ = os.WriteFile(s.Path, []byte("{broken"), 0o600)
	if _, err := s.Load(); err == nil {
		t.Fatal("a broken file must be an error")
	}
}
