package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/linearconn"
)

func TestLinearHelpAndInvalidArgumentsHaveNoEffects(t *testing.T) {
	dir := isolated(t)
	poison := filepath.Join(dir, "poison.toml")
	if err := os.WriteFile(poison, []byte("this is not config[["), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_CONFIG", poison)
	previous := newLinearManager
	defer func() { newLinearManager = previous }()
	newLinearManager = func(string) *linearconn.Manager {
		t.Fatal("help/invalid arguments created credential manager")
		return nil
	}
	for _, args := range [][]string{{"linear", "--help"}, {"linear", "connect", "--help"}, {"linear", "create", "--help"}, {"linear", "issues", "--help"}} {
		var out, diag bytes.Buffer
		if err := Run(context.Background(), args, &out, &diag); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"linear", "connect"}, {"linear", "import", "--name", "x", "--token", "secret"}, {"linear", "issues", "--profile", "x", "--limit", "0"}, {"linear", "create", "--profile", "x", "--title", "x"}, {"linear", "update", "bad", "--profile", "x", "--operation-id", "x", "--title", "a"}, {"linear", "comment", "11111111-2222-3333-4444-555555555555", "--profile", "x", "--operation-id", "x"}, {"linear", "create", "--profile", "x", "--operation-id", "x", "--title", "\x1bmalicious"}} {
		rejectCLI(t, args...)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatalf("validation touched state: %v", err)
	}
}

func TestLinearBodyFilesAndExplicitEmptyDescription(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "body")
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 65537)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLinearFile(file); err == nil {
		t.Fatal("oversize file accepted")
	}
	if _, err := readLinearFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
	v, _, err := parseLinear("update", []string{"11111111-2222-3333-4444-555555555555", "--profile", "x", "--operation-id", "x", "--description", ""}, &bytes.Buffer{})
	if err != nil || !v.supplied["description"] {
		t.Fatal("explicit empty update lost", err)
	}
}

func TestLinearAuthorityCanonicalRoutes(t *testing.T) {
	base := Config{Mode: "cloud", Server: "https://EXAMPLE.test/", Audience: "a", Principal: "Owner@Example.Test"}
	a := linearAuthority(base)
	base.Server = "https://example.test"
	base.Principal = "owner@example.test"
	if linearAuthority(base) != a {
		t.Fatal("equivalent authority routes differ")
	}
	base.Server = "https://other.test"
	if linearAuthority(base) == a {
		t.Fatal("different authority collapsed")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "state.db")
	if err := os.WriteFile(real, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.db")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if linearAuthority(Config{DB: real}) != linearAuthority(Config{DB: link}) {
		t.Fatal("local symlink authority was not canonical")
	}
}

func TestLinearDoctorSelectionRetainsExplicitProfile(t *testing.T) {
	selected := mcpSelection(Config{LinearProfile: "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: "local", DB: "db", Workspace: "w", Project: "p"}, "", "codex", "selected.toml", "clawbridge", "actor", "instance")
	if selected.LinearProfile != "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatal("doctor dropped selected profile", selected)
	}
}
