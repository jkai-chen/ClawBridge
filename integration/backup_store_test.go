package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// runBackupWith runs the backup script against an env file, with every
// external backup command faked to log and fail, and returns its output and
// the fake commands it ran.
func runBackupWith(t *testing.T, envLines ...string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the backup runner is a POSIX shell script")
	}
	runner := deploymentBackupRunner(t)
	fakeBin, runDir, deployDir := t.TempDir(), t.TempDir(), t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	for _, name := range []string{"docker", "gcloud", "gsutil", "aws", "mountpoint", "sha256sum"} {
		writeFakeCommand(t, filepath.Join(fakeBin, name), commandLog)
	}
	if err := os.MkdirAll(filepath.Join(deployDir, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deployDir, "deploy", "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(runDir, "clawbridge.env")
	base := []string{"CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data", "COMPOSE_PROJECT_NAME=clawbridge"}
	if err := os.WriteFile(envFile, []byte(strings.Join(append(base, envLines...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := backupRunnerCommand(t, runner)
	cmd.Dir = runDir
	cmd.Env = []string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + runDir, "TMPDIR=" + runDir, "LANG=C", "LC_ALL=C",
		"CLAWBRIDGE_ENV_FILE=" + envFile,
		"CLAWBRIDGE_DEPLOY_DIR=" + deployDir,
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the backup succeeded with fake commands: %s", out)
	}
	ran, _ := os.ReadFile(commandLog)
	return string(out), string(ran)
}

// The backup store is CLAWBRIDGE_BACKUP_URL (gs:// or s3:// and a bucket name,
// nothing else) or, as before, CLAWBRIDGE_BACKUP_BUCKET; a bad one fails before
// any backup command runs.
func TestBackupStoreConfigurationFailsClosed(t *testing.T) {
	for name, c := range map[string]struct {
		env  []string
		want string
	}{
		"both":                {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_BUCKET=backups"}, "not both"},
		"neither":             {nil, "CLAWBRIDGE_BACKUP_URL or CLAWBRIDGE_BACKUP_BUCKET exactly once"},
		"url twice":           {[]string{"CLAWBRIDGE_BACKUP_URL=s3://a", "CLAWBRIDGE_BACKUP_URL=s3://b"}, "CLAWBRIDGE_BACKUP_URL exactly once"},
		"other scheme":        {[]string{"CLAWBRIDGE_BACKUP_URL=https://backups.example.com"}, "gs://BUCKET or s3://BUCKET"},
		"no scheme":           {[]string{"CLAWBRIDGE_BACKUP_URL=backups"}, "gs://BUCKET or s3://BUCKET"},
		"a path":              {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups/elsewhere", "CLAWBRIDGE_BACKUP_REGION=us-east-1"}, "not a valid bucket name"},
		"a query":             {[]string{"CLAWBRIDGE_BACKUP_URL=gs://backups?x=1"}, "not a valid bucket name"},
		"empty bucket":        {[]string{"CLAWBRIDGE_BACKUP_URL=s3://", "CLAWBRIDGE_BACKUP_REGION=us-east-1"}, "not a valid bucket name"},
		"s3 without a region": {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups"}, "CLAWBRIDGE_BACKUP_REGION exactly once"},
		"s3, bad region":      {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_REGION=us east 1"}, "not an AWS region"},
		"s3, odd region":      {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_REGION=us-east-1;x"}, "not an AWS region"},
		"s3, empty name part": {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_REGION=us--1"}, "not an AWS region"},
	} {
		out, ran := runBackupWith(t, c.env...)
		if !strings.Contains(out, c.want) || ran != "" {
			t.Fatalf("%s: %q, commands run: %q", name, out, ran)
		}
	}
}

// A valid store passes the configuration and stops at a later guard: a
// command this host lacks (for s3:// that is aws, for gs:// gcloud: the store
// picks its own tool) or the data disk's mount. The script fixes its own PATH,
// so the guard it reaches depends on the host; none is a configuration error.
func TestBackupStoreConfigurationAccepted(t *testing.T) {
	for name, c := range map[string]struct {
		env  []string
		tool string
	}{
		"s3":            {[]string{"CLAWBRIDGE_BACKUP_URL=s3://clawbridge-backups-123456789012-us-east-1", "CLAWBRIDGE_BACKUP_REGION=us-east-1"}, "aws"},
		"gcs by url":    {[]string{"CLAWBRIDGE_BACKUP_URL=gs://clawbridge-backups"}, "gcloud"},
		"gcs by bucket": {[]string{"CLAWBRIDGE_BACKUP_BUCKET=clawbridge-backups"}, "gcloud"},
	} {
		out, _ := runBackupWith(t, c.env...)
		later := strings.Contains(out, "not mounted") ||
			strings.Contains(out, "unavailable: "+c.tool+"\n") ||
			strings.Contains(out, "unavailable: mountpoint") || strings.Contains(out, "unavailable: docker")
		other := map[string]string{"aws": "gcloud", "gcloud": "aws"}[c.tool]
		if !later || strings.Contains(out, "unavailable: "+other) {
			t.Fatalf("%s: %q", name, out)
		}
	}
}

// The script fixes its own PATH, so no test can run its uploads against fakes;
// this pins the exact upload commands and their order instead.
func TestBackupUploadCommands(t *testing.T) {
	raw, err := os.ReadFile(deploymentBackupRunner(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for _, want := range []string{
		`upload() { gcloud storage cp --quiet "$1" "$2" >/dev/null; }`,
		`upload() { aws s3 cp --only-show-errors --region "$region" "$1" "$2" >/dev/null; }`,
		`object_base=s3://$bucket/postgres/daily/$timestamp`,
		`object_base=gs://$bucket/postgres/daily/$timestamp`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing: %s", want)
		}
	}
	dump := strings.Index(script, `upload "$dump_file" "$object_base.dump"`)
	sum := strings.Index(script, `upload "$checksum_file" "$object_base.sha256"`)
	if dump < 0 || sum < 0 || dump > sum {
		t.Fatal("the dump must upload before its checksum")
	}
}

// runLocalBackup runs the backup runner with CLAWBRIDGE_BACKUP_DIR, a fake
// docker first on PATH (a local backup keeps the caller's PATH) that answers
// pg_isready, pg_dump and pg_restore --list, and extra environment.
func runLocalBackup(t *testing.T, backupDir string, envLines []string, extra ...string) (string, error, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the backup runner is a POSIX shell script")
	}
	runner := deploymentBackupRunner(t)
	fakeBin, runDir, deployDir := t.TempDir(), t.TempDir(), t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	docker := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + shellQuote(commandLog) + "\n" +
		"case \"$*\" in\n" +
		"*' pg_dump '*) printf 'PGDMP synthetic archive' ;;\n" +
		"*' pg_restore --list'*) cat >/dev/null ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte(docker), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gcloud", "gsutil", "aws", "mountpoint"} {
		writeFakeCommand(t, filepath.Join(fakeBin, name), commandLog)
	}
	if err := os.MkdirAll(filepath.Join(deployDir, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", "compose.local.yaml"} {
		if err := os.WriteFile(filepath.Join(deployDir, "deploy", name), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	envFile := filepath.Join(runDir, "clawbridge.env")
	if err := os.WriteFile(envFile, []byte(strings.Join(append([]string{"COMPOSE_PROJECT_NAME=cb-from-env-file"}, envLines...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := backupRunnerCommand(t, runner)
	cmd.Dir = runDir
	cmd.Env = append([]string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + runDir, "TMPDIR=" + runDir, "LANG=C", "LC_ALL=C",
		"CLAWBRIDGE_ENV_FILE=" + envFile,
		"CLAWBRIDGE_DEPLOY_DIR=" + deployDir,
		"CLAWBRIDGE_BACKUP_DIR=" + backupDir,
	}, extra...)
	out, err := cmd.CombinedOutput()
	ran, _ := os.ReadFile(commandLog)
	return string(out), err, string(ran)
}

// A local folder gets the same dump, validation and checksum, through the
// given project and override, and keeps only its own recent pairs.
func TestBackupLocalFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now().Add(-5 * 24 * time.Hour)
	for name, at := range map[string]time.Time{
		"20200101T000000Z.dump": old, "20200101T000000Z.sha256": old,
		"20200102T000000Z.dump": recent, "20200102T000000Z.sha256": recent,
		"notes.txt": old,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	out, err, ran := runLocalBackup(t, dir, nil, "CLAWBRIDGE_COMPOSE_PROJECT=cb-local-test", "CLAWBRIDGE_COMPOSE_OVERRIDE=compose.local.yaml")
	if err != nil {
		t.Fatalf("local backup: %v\n%s\n%s", err, out, ran)
	}
	if !strings.Contains(ran, "--project-name cb-local-test") || !strings.Contains(ran, filepath.Join("deploy", "compose.local.yaml")+" exec -T db pg_dump") {
		t.Fatalf("compose calls:\n%s", ran)
	}
	for _, tool := range []string{"gcloud", "aws", "mountpoint"} {
		if strings.Contains(ran, tool) {
			t.Fatalf("a local backup ran %s:\n%s", tool, ran)
		}
	}
	dumps, _ := filepath.Glob(filepath.Join(dir, "2*.dump"))
	var fresh string
	for _, d := range dumps {
		if !strings.HasPrefix(filepath.Base(d), "2020") {
			fresh = d
		}
	}
	if fresh == "" {
		t.Fatalf("no new dump: %v\n%s", dumps, out)
	}
	body, _ := os.ReadFile(fresh)
	sum := sha256.Sum256(body)
	gotSum, _ := os.ReadFile(strings.TrimSuffix(fresh, ".dump") + ".sha256")
	if string(body) != "PGDMP synthetic archive" || string(gotSum) != hex.EncodeToString(sum[:])+"  "+filepath.Base(fresh)+"\n" {
		t.Fatalf("dump %q, checksum %q", body, gotSum)
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o600 {
		t.Fatalf("dump mode %v", info.Mode())
	}
	if !strings.Contains(out, "backup="+strings.TrimSuffix(fresh, ".dump")+".dump") {
		t.Fatalf("output: %s", out)
	}
	for name, kept := range map[string]bool{
		"20200101T000000Z.dump": false, "20200101T000000Z.sha256": false,
		"20200102T000000Z.dump": true, "20200102T000000Z.sha256": true, "notes.txt": true,
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != kept {
			t.Errorf("%s: kept=%v, want %v", name, err == nil, kept)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Fatalf("partial files left: %v", left)
	}

	// The project defaults to the env file's, and a folder is made if missing.
	other := filepath.Join(t.TempDir(), "new", "backups")
	if out, err, ran = runLocalBackup(t, other, nil); err != nil || !strings.Contains(ran, "--project-name cb-from-env-file") {
		t.Fatalf("defaults: %v\n%s\n%s", err, out, ran)
	}
	if info, err := os.Stat(other); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("made folder: %v %v", info, err)
	}
}

// A bad local configuration fails before any command runs.
func TestBackupLocalFolderFailsClosed(t *testing.T) {
	for name, c := range map[string]struct {
		dir   string
		env   []string
		extra []string
		want  string
	}{
		"and a bucket":  {t.TempDir(), []string{"CLAWBRIDGE_BACKUP_URL=s3://backups"}, nil, "must not also set"},
		"and a bucket2": {t.TempDir(), []string{"CLAWBRIDGE_BACKUP_BUCKET=backups"}, nil, "must not also set"},
		"relative":      {"backups", nil, nil, "absolute path"},
		"keep zero":     {t.TempDir(), nil, []string{"CLAWBRIDGE_BACKUP_KEEP_DAYS=0"}, "KEEP_DAYS"},
		"keep words":    {t.TempDir(), nil, []string{"CLAWBRIDGE_BACKUP_KEEP_DAYS=two"}, "KEEP_DAYS"},
		"no override":   {t.TempDir(), nil, []string{"CLAWBRIDGE_COMPOSE_OVERRIDE=missing.yaml"}, "override file is missing"},
		"bad project":   {t.TempDir(), nil, []string{"CLAWBRIDGE_COMPOSE_PROJECT=Bad Project"}, "not a valid Compose project name"},
	} {
		out, err, ran := runLocalBackup(t, c.dir, c.env, c.extra...)
		if err == nil || !strings.Contains(out, c.want) || ran != "" {
			t.Errorf("%s: %v %q, commands run: %q", name, err, out, ran)
		}
	}
}
