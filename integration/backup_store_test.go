package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// runBackupWith runs the backup script against an env file, with every
// external backup command faked to log and fail, and returns its output and
// the fake commands it ran. An envLine that starts with -- is an argument to
// the runner instead.
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
	lines := []string{"CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data", "COMPOSE_PROJECT_NAME=clawbridge"}
	var args []string
	for _, line := range envLines {
		if strings.HasPrefix(line, "--") {
			args = append(args, line)
		} else {
			lines = append(lines, line)
		}
	}
	if err := os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := backupRunnerCommand(t, runner)
	cmd.Args = append(cmd.Args, args...)
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
// pg_isready, pg_dump and pg_restore --list, and extra environment; an
// extra that starts with -- is an argument to the runner instead.
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
	cmd.Env = []string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + runDir, "TMPDIR=" + runDir, "LANG=C", "LC_ALL=C",
		"CLAWBRIDGE_ENV_FILE=" + envFile,
		"CLAWBRIDGE_DEPLOY_DIR=" + deployDir,
		"CLAWBRIDGE_BACKUP_DIR=" + backupDir,
	}
	for _, e := range extra {
		if strings.HasPrefix(e, "--") {
			cmd.Args = append(cmd.Args, e)
		} else {
			cmd.Env = append(cmd.Env, e)
		}
	}
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
		"an endpoint":   {t.TempDir(), []string{"CLAWBRIDGE_BACKUP_ENDPOINT=https://s3.example.test"}, nil, "goes with an s3://"},
		"--list":        {t.TempDir(), nil, []string{"--list"}, "list it with ls"},
	} {
		out, err, ran := runLocalBackup(t, c.dir, c.env, c.extra...)
		if err == nil || !strings.Contains(out, c.want) || ran != "" {
			t.Errorf("%s: %v %q, commands run: %q", name, err, out, ran)
		}
	}
}

// Every Compose call layers compose.yaml, then compose.image.yaml when the
// server pulls the published image (CLAWBRIDGE_IMAGE), then the override.
func TestBackupComposeFiles(t *testing.T) {
	for name, c := range map[string]struct {
		env   []string
		extra []string
		files []string
	}{
		"plain":              {nil, nil, []string{"compose.yaml"}},
		"override":           {nil, []string{"CLAWBRIDGE_COMPOSE_OVERRIDE=compose.local.yaml"}, []string{"compose.yaml", "compose.local.yaml"}},
		"image":              {[]string{"CLAWBRIDGE_IMAGE=ghcr.io/example/clawbridge@sha256:" + strings.Repeat("0", 64)}, nil, []string{"compose.yaml", "compose.image.yaml"}},
		"image and override": {[]string{"CLAWBRIDGE_IMAGE=ghcr.io/example/clawbridge@sha256:" + strings.Repeat("0", 64)}, []string{"CLAWBRIDGE_COMPOSE_OVERRIDE=compose.local.yaml"}, []string{"compose.yaml", "compose.image.yaml", "compose.local.yaml"}},
	} {
		out, err, ran := runLocalBackup(t, t.TempDir(), c.env, c.extra...)
		if err != nil {
			t.Fatalf("%s: %v\n%s\n%s", name, err, out, ran)
		}
		lines := strings.Split(strings.TrimSpace(ran), "\n")
		if len(lines) != 3 {
			t.Fatalf("%s: want pg_isready, pg_dump and pg_restore, got:\n%s", name, ran)
		}
		for _, line := range lines {
			args := strings.Fields(line)
			var files []string
			for i, a := range args {
				if a == "-f" && i+1 < len(args) {
					files = append(files, filepath.Base(args[i+1]))
				}
			}
			if strings.Join(files, " ") != strings.Join(c.files, " ") || !strings.HasPrefix(line, "compose --project-name ") || !strings.Contains(line, " exec -T db ") {
				t.Fatalf("%s: Compose files %v, want %v: %s", name, files, c.files, line)
			}
		}
	}
}

// An S3-compatible store (CLAWBRIDGE_BACKUP_ENDPOINT): https://HOST[:PORT]
// and nothing else, never this host by name, with its own key; a bad one
// fails before any backup command runs.
func TestBackupEndpointConfigurationFailsClosed(t *testing.T) {
	keys := []string{"CLAWBRIDGE_BACKUP_ACCESS_KEY_ID=AKIAEXAMPLEKEY0001", "CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY=synthetic/secret+value=0123"}
	store := func(endpoint string, more ...string) []string {
		return append([]string{"CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_REGION=auto", "CLAWBRIDGE_BACKUP_ENDPOINT=" + endpoint}, more...)
	}
	for name, c := range map[string]struct {
		env  []string
		want string
	}{
		"with gs://":       {[]string{"CLAWBRIDGE_BACKUP_URL=gs://backups", "CLAWBRIDGE_BACKUP_ENDPOINT=https://s3.example.test"}, "goes with an s3://"},
		"plain http":       {store("http://s3.example.test", keys...), "must be an https:// URL"},
		"user info":        {store("https://user:pass@s3.example.test", keys...), "no user, path, query or fragment"},
		"a path":           {store("https://s3.example.test/bucket", keys...), "no user, path, query or fragment"},
		"a query":          {store("https://s3.example.test?x=1", keys...), "no user, path, query or fragment"},
		"a fragment":       {store("https://s3.example.test#x", keys...), "no user, path, query or fragment"},
		"empty port":       {store("https://s3.example.test:", keys...), "no user, path, query or fragment"},
		"no host":          {store("https://", keys...), "no user, path, query or fragment"},
		"localhost":        {store("https://localhost:9000", keys...), "is this host"},
		"loopback address": {store("https://127.0.0.1:9000", keys...), "is this host"},
		"twice":            {append(store("https://s3.example.test", keys...), "CLAWBRIDGE_BACKUP_ENDPOINT=https://other.example.test"), "CLAWBRIDGE_BACKUP_ENDPOINT exactly once"},
		"no key id":        {store("https://s3.example.test", keys[1]), "CLAWBRIDGE_BACKUP_ACCESS_KEY_ID exactly once"},
		"no secret":        {store("https://s3.example.test", keys[0]), "CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY exactly once"},
		"odd key":          {store("https://s3.example.test", "CLAWBRIDGE_BACKUP_ACCESS_KEY_ID=AKIA EXAMPLE", keys[1]), "characters a key never has"},
		"bad region":       {[]string{"CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_REGION=Auto;x", "CLAWBRIDGE_BACKUP_ENDPOINT=https://s3.example.test", keys[0], keys[1]}, "not a region name"},
	} {
		out, ran := runBackupWith(t, c.env...)
		if !strings.Contains(out, c.want) || ran != "" {
			t.Fatalf("%s: %q, commands run: %q", name, out, ran)
		}
	}
	// A valid endpoint with the store's own region name passes the
	// configuration: it needs docker, not aws.
	out, _ := runBackupWith(t, store("https://s3.us-west-004.example.test:443", keys...)...)
	if strings.Contains(out, "unavailable: aws") || strings.Contains(out, "CLAWBRIDGE_BACKUP") {
		t.Fatalf("valid endpoint: %q", out)
	}
}

// --list touches neither the database nor its disk, so it needs none of the
// dump's commands and never reaches the data disk's guard.
func TestBackupListSkipsDumpCommands(t *testing.T) {
	raw, err := os.ReadFile(deploymentBackupRunner(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `[ "$mode" = list ] || commands="$commands mountpoint date sha256sum sleep"`) {
		t.Fatal("--list requires the dump's commands")
	}
	// The endpoint keeps aws and the cloud out of it: the run stops at the
	// endpoint's name (example.test never resolves) or a command this host lacks.
	out, ran := runBackupWith(t, "--list", "CLAWBRIDGE_BACKUP_URL=s3://backups", "CLAWBRIDGE_BACKUP_REGION=auto",
		"CLAWBRIDGE_BACKUP_ENDPOINT=https://s3.example.test", "CLAWBRIDGE_BACKUP_ACCESS_KEY_ID=AKIAEXAMPLEKEY0001",
		"CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY=synthetic/secret+value=0123")
	for _, dumpOnly := range []string{"unavailable: mountpoint", "unavailable: date", "unavailable: sha256sum", "unavailable: sleep", "not mounted", "usage:"} {
		if strings.Contains(out, dumpOnly) || ran != "" {
			t.Fatalf("--list: %q, commands run: %q", out, ran)
		}
	}
}

// The endpoint's commands, pinned: the aws CLI runs in its image pinned by
// index digest; the key reaches it in an env file under /run, never argv;
// --list prints matching names only and fails when the store can't be read.
func TestBackupEndpointCommands(t *testing.T) {
	raw, err := os.ReadFile(deploymentBackupRunner(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for _, want := range []string{
		`aws_image=amazon/aws-cli@sha256:d55d5b391f1cc0d428a05eb07408bb1a730d7df9f656008f1117ed5e13b0c083`,
		`cred_dir=$(mktemp -d /run/clawbridge-backup.XXXXXX)`,
		`docker run --rm --env-file "$cred_dir/aws.env" ${work_dir:+-v "$work_dir:/backup:ro"} \`,
		`"$aws_image" --endpoint-url "$endpoint" --region "$region" "$@"`,
		`upload() { aws_run s3 cp --only-show-errors "/backup/${1##*/}" "$2" >/dev/null; }`,
		`grep -E '/postgres/daily/[0-9]{8}T[0-9]{6}Z\.(dump|sha256)$'`,
		`fail "cannot list the backup store"`,
		`set -- -f "$deploy_dir/deploy/compose.image.yaml" "$@"`,
		`127.* | ::1 | 0.* | :: | ::ffff:127.* | ::ffff:0.*) fail "the backup endpoint resolves to loopback`,
		`fe80:* | FE80:*) fail "the backup endpoint resolves to a link-local address`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing: %s", want)
		}
	}
	if n := strings.Count(script, `fail "cannot list the backup store"`); n != 3 {
		t.Fatalf("%d of the three listings fail on an unreadable store", n)
	}
	if strings.Contains(script, `--env AWS_SECRET`) || strings.Contains(script, `-e AWS_SECRET`) {
		t.Fatal("the key is on a command line")
	}
	// No command line of the script carries the key's value.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "$secret_key") && !strings.Contains(line, "printf 'AWS_ACCESS_KEY_ID=%s") && !strings.Contains(line, "read_env_value") && !strings.Contains(line, `for key_value in`) {
			t.Fatalf("the key is used on: %s", line)
		}
	}
}

// The key's private directory goes when the run ends, by any way the run
// ends: this runs the script's own cleanup and traps, with a directory in
// place of the one under /run, and exits normally, by a failure and by a
// signal.
func TestBackupCredentialDirectoryRemovedOnExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the backup runner is a POSIX shell script")
	}
	raw, err := os.ReadFile(deploymentBackupRunner(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "work_dir=\ncred_dir=\n")
	end := strings.Index(script, "trap 'exit 143' 15\n")
	if start < 0 || end < start {
		t.Fatal("the script's cleanup and traps are not where this test looks for them")
	}
	traps := script[start : end+len("trap 'exit 143' 15\n")]
	for name, finish := range map[string]string{
		"exit":    "exit 0",
		"failure": "false || exit 1",
		"signal":  "kill -TERM $$",
	} {
		credDir := filepath.Join(t.TempDir(), "clawbridge-backup.000000")
		if err := os.Mkdir(credDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(credDir, "aws.env"), []byte("AWS_ACCESS_KEY_ID=AKIAEXAMPLEKEY0001\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		body := traps + "cred_dir=" + shellQuote(credDir) + "\n" + finish + "\n"
		out, _ := exec.Command("sh", "-c", body).CombinedOutput()
		if _, err := os.Stat(credDir); !os.IsNotExist(err) {
			t.Fatalf("%s: the key's directory is still there (%v): %s", name, err, out)
		}
	}
}
