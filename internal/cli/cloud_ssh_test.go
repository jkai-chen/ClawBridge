package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The host facts script, run by sh against stub commands: one fact per
// line, the opt-out as a count, and never a value from the env file.
func TestHostFactsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on the Linux server")
	}
	script := hostFactsScriptFor("/nonexistent", "/nonexistent")
	if strings.Contains(script, "'") {
		t.Fatal("the script has a single quote")
	}
	dir := t.TempDir()
	stubs := map[string]string{
		"uname":       `case "$1" in -s) echo Linux;; -m) echo aarch64;; esac`,
		"docker":      `case "$1" in version) echo 27.3.1;; compose) echo 2.29.7;; esac`,
		"findmnt":     `echo /dev/vdb`,
		"df":          `echo "Filesystem 1024-blocks Used Available Capacity Mounted on"; echo "/dev/vdb 51474912 1024 48881488 1% /var/lib/x"`,
		"timedatectl": `echo yes`,
		"stat":        `echo 600:root`,
		"ip":          `echo "2: eth0    inet 203.0.113.7/24 brd 203.0.113.255 scope global eth0"; echo "2: eth0    inet6 2001:db8::7/64 scope global"`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := filepath.Join(dir, "clawbridge.env")
	if err := os.WriteFile(env, []byte("POSTGRES_PASSWORD=value-sentinel\nCLAWBRIDGE_DATA_ON_ROOT_DISK=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(envFile string) (hostFacts, string) {
		cmd := exec.Command("sh", "-c", hostFactsScriptFor(envFile, filepath.Join(dir, "data", "postgres")))
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "sentinel") {
			t.Fatal("a value was printed")
		}
		return parseHostFacts(out), string(out)
	}
	h, out := run(env)
	want := hostFacts{arch: "arm64", docker: true, compose: [2]int{2, 29}, mounted: true, freeKiB: 48881488, ntp: "yes", env: "600:root", rootDisk: true,
		addrs: h.addrs, seen: map[string]bool{"mount": true, "free": true}}
	if !reflect.DeepEqual(h, want) || len(h.addrs) != 2 || h.addrs[0].String() != "203.0.113.7" || h.addrs[1].String() != "2001:db8::7" {
		t.Fatalf("%+v\n%s", h, out)
	}
	if h, _ = run(filepath.Join(dir, "absent.env")); h.env != "absent" || h.rootDisk {
		t.Fatalf("absent env: %+v", h)
	}
}

// Remote output is untrusted: a line that isn't exactly a fact is left
// out, so it reads as the fact missing.
func TestParseHostFactsIsStrict(t *testing.T) {
	h := parseHostFacts([]byte("platform Darwin arm64\ndocker 27.0 extra\ncompose v2.x\nmount $(evil)\nfree lots\nfree -5\nntp maybe\nenv 600:root;rm\nrootdisk 2\naddr not-an-ip\n"))
	if !reflect.DeepEqual(h, hostFacts{seen: map[string]bool{}}) {
		t.Fatalf("%+v", h)
	}
	h = parseHostFacts([]byte("platform Linux x86_64\ndocker 27.3.1-ce\ncompose v2.24.0\nmount /dev/mapper/vg-data\n"))
	if h.arch != "amd64" || !h.docker || h.compose != [2]int{2, 24} || !h.mounted {
		t.Fatalf("%+v", h)
	}
}

func hostFactsOutput(lines ...string) string {
	base := map[string]string{
		"platform": "Linux x86_64", "docker": "27.3.1", "compose": "2.29.7", "mount": "/dev/vdb",
		"free": "48881488", "ntp": "yes", "env": "600:root", "rootdisk": "0", "addr": "203.0.113.7",
	}
	for _, l := range lines {
		k, v, _ := strings.Cut(l, " ")
		base[k] = v
	}
	var b strings.Builder
	for k, v := range base {
		if v != "-" {
			b.WriteString(k + " " + v + "\n")
		}
	}
	return b.String()
}

// infra over SSH: each failure has its own reason; the root-disk opt-out,
// a domain not on the host's interfaces and an unreadable clock are
// warnings, which pass the command.
func TestCloudCheckInfraOverSSH(t *testing.T) {
	probe := func() *cloudProbe {
		return &cloudProbe{server: "https://bridge.example.test", target: cloudTarget{host: "admin@bridge.example.test"}}
	}
	for name, c := range map[string]struct {
		facts          []string
		ips            []string
		dialErr        error
		status, reason string
	}{
		"ready":             {nil, []string{"203.0.113.7"}, nil, "pass", ""},
		"darwin":            {[]string{"platform Darwin arm64"}, nil, nil, "fail", "unsupported_platform"},
		"32-bit":            {[]string{"platform Linux armv7l"}, nil, nil, "fail", "unsupported_platform"},
		"no docker":         {[]string{"docker none", "compose none"}, nil, nil, "fail", "docker_missing"},
		"old compose":       {[]string{"compose 2.23.3"}, nil, nil, "fail", "compose_too_old"},
		"compose v1":        {[]string{"compose 1.29.2"}, nil, nil, "fail", "compose_too_old"},
		"open env file":     {[]string{"env 644:root"}, nil, nil, "fail", "env_file_mode"},
		"user env file":     {[]string{"env 600:admin"}, nil, nil, "fail", "env_file_mode"},
		"no env yet":        {[]string{"env absent", "rootdisk -"}, []string{"203.0.113.7"}, nil, "pass", ""},
		"no mount":          {[]string{"mount none"}, nil, nil, "fail", "data_not_mounted"},
		"full":              {[]string{"free 1048576"}, nil, nil, "fail", "data_disk_full"},
		"clock off":         {[]string{"ntp no"}, nil, nil, "fail", "clock_unsynced"},
		"no dns":            {nil, nil, nil, "fail", "dns_unresolved"},
		"closed":            {nil, []string{"203.0.113.7"}, errors.New("refused"), "fail", "port_closed"},
		"root disk":         {[]string{"mount none", "rootdisk 1"}, []string{"203.0.113.7"}, nil, "warn", "data_on_root_disk"},
		"behind nat":        {nil, []string{"198.51.100.9"}, nil, "warn", "dns_not_this_host"},
		"no ip command":     {[]string{"addr -"}, []string{"198.51.100.9"}, nil, "warn", "dns_not_this_host"},
		"clock unknown":     {[]string{"ntp unknown"}, []string{"203.0.113.7"}, nil, "warn", "clock_unknown"},
		"two warnings":      {[]string{"mount none", "rootdisk 1"}, []string{"198.51.100.9"}, nil, "warn", "data_on_root_disk,dns_not_this_host"},
		"opt-out not set 1": {[]string{"mount none", "rootdisk 0"}, nil, nil, "fail", "data_not_mounted"},
		"no free fact":      {[]string{"free -"}, nil, nil, "fail", "facts_incomplete"},
		"no mount fact":     {[]string{"mount -"}, nil, nil, "fail", "facts_incomplete"},
		"garbled free":      {[]string{"free 12abc"}, nil, nil, "fail", "facts_incomplete"},
	} {
		stubCloud(t, &gcloudStub{}, c.ips, c.dialErr)
		runs := stubRemote(t, hostFactsOutput(c.facts...), nil)
		got := checkInfra(context.Background(), probe())
		if got.Check != "infra" || got.Status != c.status || got.Reason != c.reason || (c.status != "pass" && got.Advice == "") {
			t.Fatalf("%s: %+v", name, got)
		}
		if len(*runs) != 1 || !reflect.DeepEqual((*runs)[0].argv, wantSSH("admin@bridge.example.test", `env LC_ALL=C sudo -n sh -c '`+hostFactsScript()+`'`)) {
			t.Fatalf("%s: runs %+v", name, *runs)
		}
	}
	// A warning passes the command; the line shows it.
	stubCloud(t, &gcloudStub{}, []string{"198.51.100.9"}, nil)
	stubRemote(t, hostFactsOutput(), nil)
	got, err := cloudCheck(t, "infra", "--server", "https://bridge.example.test", "--ssh", "admin@bridge.example.test")
	if err != nil || len(got) != 1 || got[0].Status != "warn" || got[0].Reason != "dns_not_this_host" {
		t.Fatalf("%+v %v", got, err)
	}
}

// What ssh or sudo printed names the failure: a key ssh didn't verify, or
// a sudo that wants a password; anything else stays ssh_failed. The same
// holds for a secret's write.
func TestSSHFailureReasons(t *testing.T) {
	for stderr, reason := range map[string]string{
		"Host key verification failed.\r\n":                                      "host_key_unknown",
		"@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n":   "host_key_unknown",
		"sudo: a password is required\n":                                         "sudo_needs_password",
		"sudo: a terminal is required to read the password\n":                    "sudo_needs_password",
		"ssh: connect to host bridge.example.test port 22: Connection refused\n": "ssh_failed",
	} {
		old := remoteOutput
		remoteOutput = func(_ context.Context, _ []string, w io.Writer) ([]byte, error) {
			_, _ = io.WriteString(w, stderr)
			return nil, errors.New("exit status 255")
		}
		stubCloud(t, &gcloudStub{}, []string{"203.0.113.7"}, nil)
		got := checkInfra(context.Background(), &cloudProbe{server: "https://bridge.example.test", target: cloudTarget{host: "host"}, stderr: io.Discard})
		remoteOutput = old
		if got.Reason != reason || got.Advice == "" {
			t.Fatalf("%q: %+v", stderr, got)
		}
		oldRun := runRemote
		runRemote = func(_ context.Context, _ []string, _ io.Reader, w io.Writer) error {
			_, _ = io.WriteString(w, stderr)
			return errors.New("exit status 255")
		}
		err := shellTransport{target: cloudTarget{host: "host"}}.setSecret(context.Background(), "CLAWBRIDGE_OAUTH_CLIENT_SECRET", "s3cr3t-value", nil)
		runRemote = oldRun
		if (reason == "host_key_unknown") != errors.Is(err, errHostKey) || (reason == "sudo_needs_password") != errors.Is(err, errSudoPassword) {
			t.Fatalf("%q: %v", stderr, err)
		}
	}
}

// The remote side's stderr reaches the terminal a line at a time with
// control and bidi characters escaped, and still names the failure.
func TestSSHStderrIsEscaped(t *testing.T) {
	old := remoteOutput
	t.Cleanup(func() { remoteOutput = old })
	remoteOutput = func(_ context.Context, _ []string, w io.Writer) ([]byte, error) {
		_, _ = io.WriteString(w, "\x1b]0;owned\x07banner \u202eevil\r\nHost key verification ")
		_, _ = io.WriteString(w, "failed.\nno newline at the end")
		return nil, errors.New("exit status 255")
	}
	var terminal strings.Builder
	_, err := sshRun(context.Background(), []string{"ssh"}, nil, &terminal)
	if !errors.Is(err, errHostKey) {
		t.Fatal(err)
	}
	want := "\\u001b]0;owned\\u0007banner \\u202eevil\nHost key verification failed.\nno newline at the end\n"
	if terminal.String() != want {
		t.Fatalf("%q", terminal.String())
	}
}

// A line with no newline is passed on once it passes 4 KiB, not kept.
func TestSafeLinesFlushesALongLine(t *testing.T) {
	var out strings.Builder
	s := &safeLines{w: &out}
	_, _ = s.Write([]byte(strings.Repeat("x", 5000)))
	if out.Len() != 5001 || len(s.buf) != 0 {
		t.Fatalf("%d written, %d kept", out.Len(), len(s.buf))
	}
}

// backups over SSH: the host lists its own store with its own key; only
// names that are a backup's count, and the newest complete pair must be
// under 26 hours old.
func TestCloudCheckBackupsOverSSH(t *testing.T) {
	probe := func() *cloudProbe {
		return &cloudProbe{server: "https://bridge.example.test", target: cloudTarget{host: "admin@bridge.example.test"}, stderr: io.Discard}
	}
	const base = "s3://clawbridge-backups/postgres/daily/"
	for name, c := range map[string]struct {
		out    string
		err    error
		reason string
	}{
		"fresh pair":  {base + "20261026T020000Z.dump\n" + base + "20261026T020000Z.sha256\n", nil, ""},
		"no checksum": {base + "20261026T020000Z.dump\n", nil, "no_backup"},
		"empty":       {"", nil, "no_backup"},
		"stale":       {base + "20261024T020000Z.dump\n" + base + "20261024T020000Z.sha256\n", nil, "backup_stale"},
		"hostile":     {"x " + base + "20261026T020000Z.dump\n" + "x " + base + "20261026T020000Z.sha256\n" + base + "20261026T020000Z.sha256 \x1b[2J\n" + base + "../20261026T020000Z.dump\n", nil, "no_backup"},
		"unreadable":  {"", errors.New("exit status 1"), "bucket_unreadable"},
	} {
		stubCloud(t, &gcloudStub{}, nil, nil)
		runs := stubRemote(t, c.out, c.err)
		got := checkBackups(context.Background(), probe())
		if got.Check != "backups" || got.Reason != c.reason || (c.reason == "") != (got.Status == "pass") {
			t.Fatalf("%s: %+v", name, got)
		}
		if len(*runs) != 1 || !reflect.DeepEqual((*runs)[0].argv, wantSSH("admin@bridge.example.test", `env LC_ALL=C sudo -n sh -c '`+backupListScript+`'`)) {
			t.Fatalf("%s: %+v", name, *runs)
		}
	}
	// What ssh says names the failure here too.
	old := remoteOutput
	t.Cleanup(func() { remoteOutput = old })
	remoteOutput = func(_ context.Context, _ []string, w io.Writer) ([]byte, error) {
		_, _ = io.WriteString(w, "Host key verification failed.\n")
		return nil, errors.New("exit status 255")
	}
	if got := checkBackups(context.Background(), probe()); got.Reason != "host_key_unknown" {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(backupListScript, "'") {
		t.Fatal("the script travels inside single quotes")
	}
	for _, key := range []string{"CLAWBRIDGE_BACKUP_ACCESS_KEY_ID", "CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY"} {
		if !secretKeys[key] {
			t.Fatal(key, "is not settable")
		}
	}
}
