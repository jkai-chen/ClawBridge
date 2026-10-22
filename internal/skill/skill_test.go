package skill

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/buildinfo"
)

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

// oneOf returns a helper taking an Install or Uninstall result directly.
func oneOf(t *testing.T) func([]Result, error) Result {
	return func(rs []Result, err error) Result {
		t.Helper()
		if err != nil || len(rs) != 1 {
			t.Fatal(rs, err)
		}
		return rs[0]
	}
}

// leftovers lists anything in the skills directory besides the skill itself.
func leftovers(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		if e.Name() != Name {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestEmbeddedSkillMatchesTheRepositoryFolder(t *testing.T) {
	files, _, err := embedded()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "skills", Name)
	disk, err := tree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk) != len(files) {
		t.Fatalf("embedded %d files, folder has %v", len(files), disk)
	}
	for _, p := range disk {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(b, files[p]) {
			t.Fatalf("%s differs from the embedded copy", p)
		}
	}
	if _, ok := files["SKILL.md"]; !ok {
		t.Fatal("SKILL.md not embedded at the skill root")
	}
}

func TestInstallIsIdempotentAndUpgradesAnOlderVersion(t *testing.T) {
	one := oneOf(t)
	withVersion(t, "v1.0.0")
	parent := t.TempDir()
	targets, err := Targets("", parent)
	if err != nil || targets[0].Dir != filepath.Join(parent, Name) || targets[0].Agent != "custom" {
		t.Fatal(targets, err)
	}
	r := one(Install(targets, false))
	if !r.Changed || !r.Current || r.InstalledVersion != "v1.0.0" {
		t.Fatal("first install", r)
	}
	files, want, _ := embedded()
	for p, b := range files {
		got, err := os.ReadFile(filepath.Join(parent, Name, filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("%s not installed exactly", p)
		}
	}
	var m marker
	raw, _ := os.ReadFile(filepath.Join(parent, Name, Marker))
	if json.Unmarshal(raw, &m) != nil || m.Version != "v1.0.0" || m.Digest != want || len(m.Files) != len(files) {
		t.Fatal("marker", string(raw))
	}
	before, _ := os.Stat(filepath.Join(parent, Name, "SKILL.md"))
	if r = one(Install(targets, false)); r.Changed || !r.Current {
		t.Fatal("repeat install was not a no-op", r)
	}
	after, _ := os.Stat(filepath.Join(parent, Name, "SKILL.md"))
	if !os.SameFile(before, after) {
		t.Fatal("repeat install rewrote the skill")
	}

	withVersion(t, "v1.1.0")
	s, err := Status(targets)
	if err != nil || s[0].Current || s[0].InstalledVersion != "v1.0.0" || s[0].BinaryVersion != "v1.1.0" {
		t.Fatal("older installation reported current", s, err)
	}
	if r = one(Install(targets, false)); !r.Changed || !r.Current || r.InstalledVersion != "v1.1.0" {
		t.Fatal("older unmodified installation not upgraded", r)
	}
	// A dev build keeps its version while the skill changes: the digest of
	// the embedded files decides.
	raw, _ = os.ReadFile(filepath.Join(parent, Name, Marker))
	json.Unmarshal(raw, &m)
	m.Digest = strings.Repeat("0", 64)
	raw, _ = json.Marshal(m)
	os.WriteFile(filepath.Join(parent, Name, Marker), raw, 0o644)
	if s, _ = Status(targets); s[0].Current {
		t.Fatal("a different skill under the same version reported current")
	}
	if r = one(Install(targets, false)); !r.Changed || !r.Current {
		t.Fatal("a different skill under the same version not replaced", r)
	}
	if l := leftovers(t, parent); len(l) != 0 {
		t.Fatal("staging directories left behind", l)
	}
}

func TestInstallRefusesChangedOrForeignFilesWithoutForce(t *testing.T) {
	one := oneOf(t)
	withVersion(t, "v1.0.0")
	parent := t.TempDir()
	targets, _ := Targets("", parent)
	one(Install(targets, false))
	skill := filepath.Join(parent, Name, "SKILL.md")
	if err := os.WriteFile(skill, []byte("my edits"), 0o644); err != nil {
		t.Fatal(err)
	}
	withVersion(t, "v1.1.0")
	r := one(Install(targets, false))
	if r.Changed || r.Refused == "" || !reflect.DeepEqual(r.Modified, []string{"SKILL.md"}) {
		t.Fatal("changed file not reported", r)
	}
	if b, _ := os.ReadFile(skill); string(b) != "my edits" {
		t.Fatal("changed file overwritten without --force")
	}
	if r = one(Install(targets, true)); !r.Changed || !r.Current {
		t.Fatal("--force did not replace", r)
	}

	// A file the user added beside our installation would be lost by the swap,
	// so an upgrade is refused until --force.
	withVersion(t, "v1.2.0")
	notes := filepath.Join(parent, Name, "notes.md")
	os.WriteFile(notes, []byte("user notes"), 0o644)
	if r = one(Install(targets, false)); r.Changed || r.Refused == "" || !reflect.DeepEqual(r.Unmanaged, []string{"notes.md"}) {
		t.Fatal("added file not reported", r)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Fatal("added file removed without --force", err)
	}

	// A directory we never installed is not ours to replace.
	foreign := t.TempDir()
	if err := os.MkdirAll(filepath.Join(foreign, Name), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(foreign, Name, "SKILL.md"), []byte("someone else's skill"), 0o644)
	targets, _ = Targets("", foreign)
	if r = one(Install(targets, false)); r.Changed || r.Refused == "" || !reflect.DeepEqual(r.Unmanaged, []string{"SKILL.md"}) {
		t.Fatal("foreign skill not refused", r)
	}
	if r = one(Install(targets, true)); !r.Changed || !r.Current || len(r.Unmanaged) != 0 {
		t.Fatal("--force did not replace the foreign skill", r)
	}
}

func TestUninstallRemovesOnlyOurFiles(t *testing.T) {
	one := oneOf(t)
	parent := t.TempDir()
	targets, _ := Targets("", parent)
	one(Install(targets, false))
	mine := filepath.Join(parent, Name, "references", "notes.md")
	os.WriteFile(mine, []byte("user notes"), 0o644)
	r := one(Uninstall(targets, false))
	if !r.Changed || r.Installed {
		t.Fatal("uninstall", r)
	}
	left, _ := tree(filepath.Join(parent, Name))
	if !reflect.DeepEqual(left, []string{"references/notes.md"}) {
		t.Fatal("uninstall removed user files or left ours", left)
	}

	parent = t.TempDir()
	targets, _ = Targets("", parent)
	one(Install(targets, false))
	os.WriteFile(filepath.Join(parent, Name, "SKILL.md"), []byte("my edits"), 0o644)
	if r = one(Uninstall(targets, false)); r.Changed || r.Refused == "" {
		t.Fatal("changed file removed without --force", r)
	}
	if r = one(Uninstall(targets, true)); !r.Changed {
		t.Fatal("--force uninstall", r)
	}
	if _, err := os.Stat(filepath.Join(parent, Name)); !os.IsNotExist(err) {
		t.Fatal("empty skill directory left behind", err)
	}

	// Without our marker nothing is removed.
	parent = t.TempDir()
	os.MkdirAll(filepath.Join(parent, Name), 0o755)
	os.WriteFile(filepath.Join(parent, Name, "SKILL.md"), []byte("someone else's skill"), 0o644)
	targets, _ = Targets("", parent)
	if r = one(Uninstall(targets, true)); r.Changed || r.Refused == "" {
		t.Fatal("unmarked skill removed", r)
	}
	if b, _ := os.ReadFile(filepath.Join(parent, Name, "SKILL.md")); string(b) != "someone else's skill" {
		t.Fatal("unmarked skill changed")
	}
}

func TestTargetsUseEachAgentsConfigurationHome(t *testing.T) {
	claude, codex := t.TempDir(), filepath.Join(t.TempDir(), "absent")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	got, err := Targets("", "")
	if err != nil || !reflect.DeepEqual(got, []Target{{"claude-code", filepath.Join(claude, "skills", Name)}}) {
		t.Fatal("default must pick only agents that are configured here", got, err)
	}
	got, err = Targets("all", "")
	if err != nil || len(got) != 2 || got[1] != (Target{"codex", filepath.Join(codex, "skills", Name)}) {
		t.Fatal("all", got, err)
	}
	if got, err = Targets("codex", ""); err != nil || len(got) != 1 || got[0].Agent != "codex" {
		t.Fatal("an explicit agent is used even before its first run", got, err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	if _, err = Targets("", ""); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Fatal("no configured agent must ask for --agent or --dir", err)
	}
	t.Setenv("CODEX_HOME", "relative")
	if _, err = Targets("codex", ""); err == nil {
		t.Fatal("relative CODEX_HOME accepted")
	}
	if _, err = Targets("other", ""); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestSkillCommandReportsJSONAndFailsOnRefusal(t *testing.T) {
	parent := t.TempDir()
	var out, errs bytes.Buffer
	if err := RunWithIO([]string{"install", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	var res struct {
		Source string   `json:"source"`
		Skills []Result `json:"skills"`
	}
	if json.Unmarshal(out.Bytes(), &res) != nil || res.Source != "skill" || len(res.Skills) != 1 || !res.Skills[0].Changed {
		t.Fatal(out.String())
	}
	os.WriteFile(filepath.Join(parent, Name, "SKILL.md"), []byte("my edits"), 0o644)
	out.Reset()
	if err := RunWithIO([]string{"uninstall", "--dir", parent}, &out, &errs); err == nil || !strings.Contains(out.String(), `"refused"`) {
		t.Fatal("refusal must be reported and fail", err, out.String())
	}
	if err := RunWithIO([]string{"status", "--force", "--dir", parent}, &out, &errs); err == nil {
		t.Fatal("status accepted --force")
	}
	if err := RunWithIO([]string{"remove"}, &out, &errs); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}
