package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	for name := range bundled {
		files, _, err := embedded(name)
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join("..", "..", "skills", name)
		disk, err := tree(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(disk) != len(files) {
			t.Fatalf("%s: embedded %d files, folder has %v", name, len(files), disk)
		}
		for _, p := range disk {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil || !bytes.Equal(b, files[p]) {
				t.Fatalf("%s/%s differs from the embedded copy", name, p)
			}
		}
		// The agent finds a skill by its frontmatter name, which must be its
		// directory name.
		if !bytes.HasPrefix(files["SKILL.md"], []byte("---\nname: "+name+"\n")) {
			t.Fatalf("%s: SKILL.md does not start with its name", name)
		}
	}
	// Every playbook is bundled.
	for _, name := range Playbooks {
		if _, ok := bundled[name]; !ok {
			t.Fatalf("playbook %s is not bundled", name)
		}
	}
}

// A playbook installs beside the main skill, under its own name, and is
// managed on its own: its status, upgrade and uninstall leave the main
// skill alone.
func TestPlaybookInstallsBesideTheSkill(t *testing.T) {
	parent := t.TempDir()
	var out, errs bytes.Buffer
	if err := RunWithIO([]string{"install", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	if err := RunWithIO([]string{"install", "--playbook", "gcp", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	for _, name := range []string{Name, "clawbridge-provision-gcp"} {
		if _, err := os.Stat(filepath.Join(parent, name, "SKILL.md")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	gcp, err := TargetsFor("clawbridge-provision-gcp", "", parent)
	if err != nil {
		t.Fatal(err)
	}
	states, err := Status(gcp)
	if err != nil || len(states) != 1 || !states[0].Current || states[0].Corrupt != "" {
		t.Fatalf("%+v %v", states, err)
	}
	out.Reset()
	if err := RunWithIO([]string{"uninstall", "--playbook", "gcp", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	if _, err := os.Stat(filepath.Join(parent, "clawbridge-provision-gcp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the playbook was not removed")
	}
	main, _ := Targets("", parent)
	if states, err := Status(main); err != nil || !states[0].Current {
		t.Fatalf("the main skill changed: %+v %v", states, err)
	}
	if err := RunWithIO([]string{"install", "--playbook", "azure", "--dir", parent}, &out, &errs); err == nil || !strings.Contains(err.Error(), "--playbook must be gcp, aws or local") {
		t.Fatal(err)
	}
	// The AWS playbook installs and uninstalls the same way, on its own.
	if err := RunWithIO([]string{"install", "--playbook", "aws", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	if _, err := os.Stat(filepath.Join(parent, "clawbridge-provision-aws", "references", "commands.md")); err != nil {
		t.Fatal(err)
	}
	if err := RunWithIO([]string{"uninstall", "--playbook", "aws", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	if _, err := TargetsFor("unknown", "", parent); err == nil {
		t.Fatal("an unknown skill resolved")
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
	files, want, _ := embedded(Name)
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
	if r = one(Uninstall(targets, true)); !r.Changed || r.Backup == "" {
		t.Fatal("--force uninstall", r)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Backup, "SKILL.md")); string(b) != "my edits" {
		t.Fatal("--force uninstall deleted the edited file instead of backing it up")
	}
	if entries, _ := os.ReadDir(r.Backup); len(entries) != 1 {
		t.Fatal("unedited files were backed up too", entries)
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
	if _, err = Targets("", ""); err == nil || err.Error() != "No Claude Code or Codex installation found; install one, then run `clawbridge skill install` (or pass --dir PATH for a custom agent folder)" || strings.Contains(err.Error(), "--agent") {
		t.Fatal("no configured agent must say to install one first, without --agent advice", err)
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

// skillsHome lays out AGENT_HOME/skills like an agent, returning both.
func skillsHome(t *testing.T) (home, skills string) {
	home = t.TempDir()
	skills = filepath.Join(home, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, skills
}

func readMarker(t *testing.T, dir string) map[string]any {
	t.Helper()
	m := map[string]any{}
	raw, _ := os.ReadFile(filepath.Join(dir, Marker))
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeMarker(t *testing.T, dir string, m map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, Marker), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAMarkerNamingAPathOutsideTheSkillIsNeverTrusted(t *testing.T) {
	one := oneOf(t)
	home, skills := skillsHome(t)
	targets, _ := Targets("", skills)
	dir := targets[0].Dir
	one(Install(targets, false))
	victim := filepath.Join(home, "victim.txt")
	os.WriteFile(victim, []byte("keep me"), 0o644)
	for _, bad := range []string{"../../victim.txt", "/" + victim, "references/../../../victim.txt", "references/./cli.md", "references\\cli.md", Marker} {
		m := readMarker(t, dir)
		m["files"].(map[string]any)[bad] = sum([]byte("keep me"))
		writeMarker(t, dir, m)
		s, err := Status(targets)
		if err != nil || s[0].Corrupt == "" || s[0].Installed {
			t.Fatalf("%s: marker trusted: %+v %v", bad, s, err)
		}
		for _, force := range []bool{false, true} {
			if r := one(Uninstall(targets, force)); r.Changed || r.Refused == "" {
				t.Fatalf("%s: uninstall with an untrusted marker: %+v", bad, r)
			}
		}
		if r := one(Install(targets, false)); r.Changed || r.Refused == "" {
			t.Fatalf("%s: install over an untrusted marker: %+v", bad, r)
		}
		if b, err := os.ReadFile(victim); err != nil || string(b) != "keep me" {
			t.Fatalf("%s: file outside the skill touched", bad)
		}
		delete(m["files"].(map[string]any), bad)
		writeMarker(t, dir, m)
	}
	writeMarker(t, dir, map[string]any{"files": "not a map"})
	if s, _ := Status(targets); s[0].Corrupt == "" {
		t.Fatal("unreadable marker trusted")
	}
	many := map[string]any{}
	for n := 0; n <= maxMarkerFiles; n++ {
		many[fmt.Sprintf("f%d.md", n)] = sum(nil)
	}
	writeMarker(t, dir, map[string]any{"files": many})
	if s, _ := Status(targets); s[0].Corrupt == "" {
		t.Fatal("unbounded marker trusted")
	}
}

// staleSetup installs, then makes the marker look like an older version that
// also shipped old.md, which this binary no longer embeds.
func staleSetup(t *testing.T, oldContent, onDisk string) ([]Target, string) {
	t.Helper()
	_, skills := skillsHome(t)
	targets, _ := Targets("", skills)
	dir := targets[0].Dir
	oneOf(t)(Install(targets, false))
	m := readMarker(t, dir)
	m["files"].(map[string]any)["old.md"] = sum([]byte(oldContent))
	m["version"] = "v0.0.1"
	writeMarker(t, dir, m)
	os.WriteFile(filepath.Join(dir, "old.md"), []byte(onDisk), 0o644)
	return targets, dir
}

func TestAnUpgradeRemovesAStaleFileTheOldVersionInstalled(t *testing.T) {
	one := oneOf(t)
	targets, dir := staleSetup(t, "old skill text", "old skill text")
	s, _ := Status(targets)
	if s[0].Corrupt != "" || !s[0].Installed || len(s[0].Unmanaged) != 0 || len(s[0].Modified) != 0 {
		t.Fatalf("a stale entry is not corruption: %+v", s[0])
	}
	r := one(Install(targets, false))
	if !r.Changed || !r.Current || r.Backup != "" {
		t.Fatalf("plain upgrade: %+v", r)
	}
	if _, err := os.Lstat(filepath.Join(dir, "old.md")); !os.IsNotExist(err) {
		t.Fatal("stale file left behind", err)
	}
}

func TestAChangedStaleFileIsRefusedThenBackedUp(t *testing.T) {
	one := oneOf(t)
	targets, dir := staleSetup(t, "old skill text", "my edits")
	r := one(Install(targets, false))
	if r.Changed || r.Refused == "" || !reflect.DeepEqual(r.Modified, []string{"old.md"}) {
		t.Fatalf("changed stale file not refused: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "old.md")); string(b) != "my edits" {
		t.Fatal("changed stale file touched")
	}
	r = one(Install(targets, true))
	if !r.Changed || !r.Current || r.Backup == "" {
		t.Fatalf("--force: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Backup, "old.md")); string(b) != "my edits" {
		t.Fatal("backup lost the changed stale file")
	}
}

func TestALinkInsideTheSkillIsNeverFollowed(t *testing.T) {
	one := oneOf(t)
	home, skills := skillsHome(t)
	targets, _ := Targets("", skills)
	dir := targets[0].Dir
	one(Install(targets, false))
	// Move references outside and link it back: hashes still match through
	// the link, but nothing may be read or removed through it.
	outside := filepath.Join(home, "outside")
	if err := os.Rename(filepath.Join(dir, "references"), outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "references")); err != nil {
		t.Fatal(err)
	}
	before, _ := tree(outside)
	s, _ := Status(targets)
	if s[0].Current || !slices.Contains(s[0].Unsafe, "references") {
		t.Fatalf("link not reported: %+v", s[0])
	}
	for _, force := range []bool{false, true} {
		if r := one(Uninstall(targets, force)); r.Changed || r.Refused == "" {
			t.Fatalf("uninstall through a link: %+v", r)
		}
	}
	if r := one(Install(targets, false)); r.Changed || r.Refused == "" {
		t.Fatalf("install over a link: %+v", r)
	}
	r := one(Install(targets, true))
	if !r.Changed || !r.Current || r.Backup == "" {
		t.Fatalf("--force must move the linked folder to a backup: %+v", r)
	}
	if after, _ := tree(outside); !reflect.DeepEqual(before, after) {
		t.Fatal("files behind the link changed", before, after)
	}
	if target, err := os.Readlink(filepath.Join(r.Backup, "references")); err != nil || target != outside {
		t.Fatal("backup lost the link", target, err)
	}
	if !strings.HasPrefix(r.Backup, home+string(filepath.Separator)) || strings.HasPrefix(r.Backup, skills) {
		t.Fatal("backup must sit beside the skills directory, not in it", r.Backup)
	}
}

func TestForceMovesForeignFilesToABackupAndNeverDeletesThem(t *testing.T) {
	one := oneOf(t)
	for name, prepare := range map[string]func(t *testing.T, dir string){
		"unmarked folder": func(t *testing.T, dir string) {
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("someone else's skill"), 0o644)
		},
		"added file": func(t *testing.T, dir string) {
			one(Install([]Target{{Agent: "custom", Dir: dir}}, false))
			os.WriteFile(filepath.Join(dir, "notes.md"), []byte("user notes"), 0o644)
		},
		"corrupt marker": func(t *testing.T, dir string) {
			one(Install([]Target{{Agent: "custom", Dir: dir}}, false))
			os.WriteFile(filepath.Join(dir, Marker), []byte("{"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, skills := skillsHome(t)
			targets, _ := Targets("", skills)
			dir := targets[0].Dir
			prepare(t, dir)
			before, _ := tree(dir)
			r := one(Install(targets, true))
			if !r.Changed || !r.Current || r.Backup == "" {
				t.Fatalf("%+v", r)
			}
			if after, _ := tree(r.Backup); !reflect.DeepEqual(before, after) {
				t.Fatal("backup is not the previous folder", before, after)
			}
			entries, _ := os.ReadDir(skills)
			if len(entries) != 1 || entries[0].Name() != Name {
				t.Fatal("the backup would load as a second skill", entries)
			}
		})
	}
}

func TestRemoveEmptyDirsLeavesALinkedDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "references")); err != nil {
		t.Fatal(err)
	}
	removeEmptyDirs(dir, []string{"references/cli.md"})
	if _, err := os.Lstat(filepath.Join(dir, "references")); err != nil {
		t.Fatal("a linked directory was removed", err)
	}
}

// The AWS playbook keeps the six hard rules of every playbook
// (ONBOARDING-DESIGN §3, §13), and its embedded JSON is exactly what the
// release ships in deploy/, so the two cannot drift apart.
func TestAWSPlaybookMatchesTheRulesAndTheDeployFiles(t *testing.T) {
	read := func(parts ...string) string {
		raw, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	titles := func(skill string) []string {
		var out []string
		rules := skill[strings.Index(skill, "## Hard rules"):]
		rules = rules[:strings.Index(rules[3:], "\n## ")+3]
		for _, line := range strings.Split(rules, "\n") {
			if len(line) > 3 && line[0] >= '1' && line[0] <= '6' && strings.HasPrefix(line[1:], ". **") {
				out = append(out, line[:strings.Index(line[4:], "**")+6])
			}
		}
		return out
	}
	gcp, aws := titles(read("skills", "clawbridge-provision-gcp", "SKILL.md")), titles(read("skills", "clawbridge-provision-aws", "SKILL.md"))
	want := []string{"1. **Explicit profile and region.**", "2. **Cost first.**", "3. **Secrets stay out of your hands.**",
		"4. **Ask before every delete.**", "5. **Stop on mismatch.**", "6. **The check decides.**"}
	if strings.Join(aws, "|") != strings.Join(want, "|") || strings.Join(gcp[1:], "|") != strings.Join(want[1:], "|") {
		t.Fatalf("hard rules: aws %q, gcp %q", aws, gcp)
	}
	commands := read("skills", "clawbridge-provision-aws", "references", "commands.md")
	for _, file := range []string{"s3-lifecycle.json", "aws-instance-role-policy.json"} {
		if !strings.Contains(commands, "```json\n"+read("deploy", file)+"```") {
			t.Fatalf("commands.md does not carry deploy/%s exactly", file)
		}
	}
	for _, rule := range []string{"--profile PROFILE --region REGION", "AWS-RunShellScript", "never put a secret"} {
		if !strings.Contains(read("skills", "clawbridge-provision-aws", "SKILL.md")+commands, rule) {
			t.Fatalf("missing: %s", rule)
		}
	}
}

// The local playbook installs like the others, and holds its hard rules and
// deploy/local-host.sh's fixed commands.
func TestLocalPlaybook(t *testing.T) {
	parent := t.TempDir()
	var out, errs bytes.Buffer
	if err := RunWithIO([]string{"install", "--playbook", "local", "--dir", parent}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	for _, p := range []string{"SKILL.md", "LICENSE", "agents/openai.yaml", "references/signin.md"} {
		if _, err := os.Stat(filepath.Join(parent, "clawbridge-provision-local", filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "skills", "clawbridge-provision-local", filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	skill, signin := read("SKILL.md"), read("references/signin.md")
	for _, want := range []string{"1. **No installs without asking.**", "2. **Never use sudo.**", "4. **Secrets stay out of your hands.**",
		"5. **Stop on mismatch.**", "6. **The check decides.**", "grep -c '^KEY=.' ~/.clawbridge-server/clawbridge.env",
		"git clone --depth 1 --branch vX.Y.Z https://github.com/jkai-chen/ClawBridge clawbridge",
		"local-host.sh up --lan clawbridge.lan --bind 192.0.2.10 --owner owner@example.test --workspace my-team </dev/null", "with `</dev/null`, and never in an interactive",
		"clawbridge cloud check server version --local --server http://127.0.0.1:8443", "clawbridge cloud check trust --local", "untrusted_ca"} {
		if !strings.Contains(skill, want) {
			t.Fatalf("SKILL.md misses %q", want)
		}
	}
	for _, want := range []string{"sh clawbridge/deploy/local-host.sh trust", "sh clawbridge/deploy/local-host.sh invite-owner --workspace my-team",
		"clawbridge join --server http://127.0.0.1:8443 --email owner@example.test", "clawbridge onboard", "192.0.2.10 clawbridge.lan"} {
		if !strings.Contains(signin, want) {
			t.Fatalf("signin.md misses %q", want)
		}
	}
}
