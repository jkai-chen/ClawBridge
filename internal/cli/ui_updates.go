package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/guibackend"
	"clawbridge/internal/selfupdate"
	"clawbridge/internal/skill"
)

// uiUpdateCheck is `self-update --check`'s logic; tests replace it so they
// make no request.
var uiUpdateCheck = func(ctx context.Context) (selfupdate.CheckResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return selfupdate.Check(ctx, selfupdate.Options{Current: buildinfo.Version})
}

// diskVersionEnv is the whole environment the on-disk binary's `version`
// runs with: nothing it does not need (no tokens, no profile).
func diskVersionEnv(environ []string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "SYSTEMROOT": true, "USERPROFILE": true}
	out := []string{"CLAWBRIDGE_OUTPUT=json"}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return out
}

// uiRunVersion runs `<exe> version` and returns the version it reports.
var uiRunVersion = func(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "version")
	cmd.Env = diskVersionEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var v struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err = json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	return v.Data.Version, nil
}

// diskVersions remembers the answer for the process life, keyed by the
// file's path, modification time and size: a new binary is a new key.
type diskVersions struct {
	mu      sync.Mutex
	key     string
	version string
	have    bool
}

func (d *diskVersions) get(ctx context.Context, exe string, info os.FileInfo, run func(context.Context, string) (string, error)) (string, error) {
	key := fmt.Sprintf("%s|%d|%d", exe, info.ModTime().UnixNano(), info.Size())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.have && d.key == key {
		return d.version, nil
	}
	v, err := run(ctx, exe)
	if err != nil {
		return "", err
	}
	d.key, d.version, d.have = key, v, true
	return v, nil
}

var uiDiskCache diskVersions

// uiDiskVersion is the version of the clawbridge file on disk, which is
// newer than the running one after an update.
var uiDiskVersion = func(ctx context.Context) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	return uiDiskCache.get(ctx, exe, info, uiRunVersion)
}

// uiSkillRows reads the installed skill of every agent that has a home.
var uiSkillRows = func() ([]guibackend.SkillRow, error) {
	targets, err := skill.Targets("", "")
	if err != nil {
		if err.Error() == skill.NoAgentFound {
			return nil, nil
		}
		return nil, err
	}
	states, err := skill.Status(targets)
	if err != nil {
		return nil, err
	}
	rows := make([]guibackend.SkillRow, 0, len(states))
	for _, s := range states {
		rows = append(rows, guibackend.SkillRow{Agent: s.Agent, Installed: s.Installed, InstalledVersion: s.InstalledVersion, Current: s.Current})
	}
	return rows, nil
}

// uiUpdatesConfig wires the Updates reads. The check is the only network
// call and runs only when the page asks for it.
func uiUpdatesConfig(in *uiInputs) guibackend.UpdatesConfig {
	return guibackend.UpdatesConfig{
		ClientVersion: buildinfo.Current().Version,
		DiskVersion:   uiDiskVersion,
		Skill:         uiSkillRows,
		Doctor:        in.Doctor,
		Check:         uiUpdateCheck,
		AgentMarker:   func() string { return agentMarker(closesEnv()) },
	}
}
