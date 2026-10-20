package integration

import (
	"context"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
)

type doctorProbeExecutor struct {
	projects any
}

func (e doctorProbeExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{
		Source: "local-authority",
		Data:   map[string]any{"projects": e.projects},
	}, nil
}

func (doctorProbeExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "workspace-id", "project-id", nil
}

func TestDoctorAcceptsEquivalentProjectListRepresentations(t *testing.T) {
	config := doctor.Config{
		Path:    filepath.Join(t.TempDir(), "config.toml"),
		Present: true, Valid: true, Mode: "local",
		DB: filepath.Join(t.TempDir(), "local.db"), Workspace: "workspace",
	}
	for name, projects := range map[string]any{
		"interface-slice": []any{map[string]any{"id": "project-id", "slug": "project"}},
		"typed-slice":     []map[string]any{{"id": "project-id", "slug": "project"}},
	} {
		t.Run(name, func(t *testing.T) {
			report := doctor.Run(context.Background(), doctor.Input{Config: config}, doctorProbeExecutor{projects: projects})
			for _, check := range report.Checks {
				if check.ID != "authority.health" {
					continue
				}
				if check.Status != doctor.Pass {
					t.Fatalf("authority status=%s: %s", check.Status, check.Message)
				}
				return
			}
			t.Fatal("doctor did not emit authority.health")
		})
	}
}
