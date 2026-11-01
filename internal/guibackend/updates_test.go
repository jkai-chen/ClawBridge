package guibackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/doctor"
	"clawbridge/internal/selfupdate"
)

func fixtureUpdatesCfg() UpdatesConfig {
	return UpdatesConfig{
		ClientVersion: "v1.0.0",
		DiskVersion:   func(context.Context) (string, error) { return "v1.0.1", nil },
		Skill: func() ([]SkillRow, error) {
			return []SkillRow{{Agent: "claude-code", Installed: true, InstalledVersion: "v1.0.0", Current: true}, {Agent: "codex", Installed: true, InstalledVersion: "v0.9.4", Current: false}}, nil
		},
		Doctor: syntheticDoctor,
		Check: func(context.Context) (selfupdate.CheckResult, error) {
			return selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.0", Target: "v1.1.0", Action: selfupdate.Update}, ManualRequired: true,
				ManualText: "v1.1.0 needs a manual update: sign in again after installing."}, nil
		},
		AgentMarker: func() string { return "" },
		Now:         func() time.Time { return time.Date(2026, 1, 2, 10, 42, 0, 0, time.UTC) },
	}
}

func callM(t *testing.T, m Method) map[string]any {
	t.Helper()
	out, err := m.Call(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var v map[string]any
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUpdatesStatus(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	cfg.Check = func(context.Context) (selfupdate.CheckResult, error) {
		t.Error("updates.status reached the network")
		return selfupdate.CheckResult{}, nil
	}
	m := UpdatesStatus(cfg)
	if m.Class != Read {
		t.Fatal("not Read-class")
	}
	v := callM(t, m)
	client := v["client"].(map[string]any)
	if client["version"] != "v1.0.0" || client["on_disk"] != "v1.0.1" || client["restart_needed"] != true {
		t.Errorf("client %v", client)
	}
	if skills := v["skill"].([]any); len(skills) != 2 || skills[1].(map[string]any)["installed_version"] != "v0.9.4" {
		t.Errorf("skill %v", v["skill"])
	}
	mcp := v["mcp"].([]any)
	if len(mcp) != 1 || mcp[0].(map[string]any)["agent"] != "codex" || mcp[0].(map[string]any)["name"] != "clawbridge" || mcp[0].(map[string]any)["status"] != "warn" {
		t.Errorf("mcp %v", mcp)
	}
	if v["auto"].(map[string]any)["available"] != false {
		t.Errorf("auto %v", v["auto"])
	}
	if _, has := v["gui"]; has {
		t.Error("a GUI app row exists (§12.5)")
	}
}

func TestUpdatesStatusSameVersionOnDiskNeedsNoRestart(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	cfg.DiskVersion = func(context.Context) (string, error) { return "v1.0.0", nil }
	if c := callM(t, UpdatesStatus(cfg))["client"].(map[string]any); c["restart_needed"] != false {
		t.Errorf("client %v", c)
	}
	cfg.DiskVersion = func(context.Context) (string, error) { return "", errors.New("no") }
	if c := callM(t, UpdatesStatus(cfg))["client"].(map[string]any); c["restart_needed"] != false || c["on_disk"] != "" {
		t.Errorf("client %v", c)
	}
	cfg.ClientVersion = "dev"
	cfg.DiskVersion = func(context.Context) (string, error) { return "v1.0.1", nil }
	if c := callM(t, UpdatesStatus(cfg))["client"].(map[string]any); c["restart_needed"] != false {
		t.Errorf("a development build is told to restart: %v", c)
	}
}

func TestUpdatesStatusReadsFailuresAsNotes(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	cfg.Skill = func() ([]SkillRow, error) { return nil, errors.New("skill home unreadable") }
	cfg.Doctor = func(context.Context) (doctor.Report, error) { return doctor.Report{}, errors.New("doctor failed") }
	v := callM(t, UpdatesStatus(cfg))
	if !strings.Contains(v["skill_note"].(string), "unreadable") || !strings.Contains(v["mcp_note"].(string), "doctor failed") {
		t.Errorf("%v", v)
	}
}

func TestUpdatesCheckCases(t *testing.T) {
	cases := []struct {
		name          string
		res           selfupdate.CheckResult
		avail, manual bool
		hold          bool
	}{
		{"up to date", selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.0", Target: "v1.0.0", Action: selfupdate.UpToDate}}, false, false, false},
		{"available", selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.0", Target: "v1.0.1", Action: selfupdate.Update}}, true, false, false},
		{"manual", selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.0", Target: "v1.1.0", Action: selfupdate.Update}, ManualRequired: true, ManualText: "v1.1.0 needs a manual update"}, true, true, false},
		{"hold", selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.0", Target: "v1.0.2", Action: selfupdate.Update}, Hold: true}, true, false, true},
		{"older latest", selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.1", Target: "v1.0.0", Action: selfupdate.DowngradeRefused}}, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := fixtureUpdatesCfg()
			cfg.Check = func(context.Context) (selfupdate.CheckResult, error) { return c.res, nil }
			v := callM(t, UpdatesCheck(cfg))
			if v["update_available"] != c.avail || v["manual_required"] != c.manual || v["hold"] != c.hold || v["target"] != c.res.Target || v["checked_at"] != "2026-01-02T10:42:00Z" {
				t.Errorf("%v", v)
			}
		})
	}
}

func TestUpdatesCheckRefusedUnderAnAgentMarker(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	cfg.AgentMarker = func() string { return "CLAUDECODE" }
	cfg.Check = func(context.Context) (selfupdate.CheckResult, error) {
		t.Error("the network was reached under an agent marker")
		return selfupdate.CheckResult{}, nil
	}
	_, err := UpdatesCheck(cfg).Call(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdatesCheckError(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	cfg.Check = func(context.Context) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{}, errors.New("latest release discovery failed")
	}
	if _, err := UpdatesCheck(cfg).Call(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdatesCheckIsCachedForAMinute(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	clock := time.Date(2026, 1, 2, 10, 42, 0, 0, time.UTC)
	cfg.Now = func() time.Time { return clock }
	calls := 0
	cfg.Check = func(context.Context) (selfupdate.CheckResult, error) {
		calls++
		return selfupdate.CheckResult{Plan: selfupdate.Plan{Current: "v1.0.0", Target: "v1.0.1", Action: selfupdate.Update}}, nil
	}
	m := UpdatesCheck(cfg)
	first := callM(t, m)
	clock = clock.Add(59 * time.Second)
	second := callM(t, m)
	if calls != 1 || first["cached"] != false || second["cached"] != true || second["checked_at"] != "2026-01-02T10:42:00Z" || second["target"] != "v1.0.1" {
		t.Fatalf("calls %d, first %v, second %v", calls, first, second)
	}
	clock = clock.Add(2 * time.Second)
	if third := callM(t, m); calls != 2 || third["cached"] != false || third["checked_at"] != "2026-01-02T10:43:01Z" {
		t.Fatalf("calls %d, third %v", calls, third)
	}
}

func TestUpdatesCheckCacheKeepsNoFailuresAndStillRefusesAgents(t *testing.T) {
	cfg := fixtureUpdatesCfg()
	fail, calls := true, 0
	cfg.Check = func(context.Context) (selfupdate.CheckResult, error) {
		calls++
		if fail {
			return selfupdate.CheckResult{}, errors.New("offline")
		}
		return selfupdate.CheckResult{Plan: selfupdate.Plan{Action: selfupdate.UpToDate}}, nil
	}
	marker := ""
	cfg.AgentMarker = func() string { return marker }
	m := UpdatesCheck(cfg)
	if _, err := m.Call(context.Background(), nil); err == nil {
		t.Fatal("no error")
	}
	fail = false
	callM(t, m)
	if calls != 2 {
		t.Fatalf("a failure was cached: %d calls", calls)
	}
	marker = "CLAUDECODE"
	if _, err := m.Call(context.Background(), nil); err == nil {
		t.Fatal("a cached answer was served to an agent")
	}
}
