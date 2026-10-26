package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
