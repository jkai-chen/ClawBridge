package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// cloud check --local: the server run on this machine by deploy/local-host.sh
// (docs/operations/local-host.md). The script's names are fixed there: the
// env file is DIR/clawbridge.env and backups go to DIR/backups.

const localEnvName = "clawbridge.env"

var (
	localPortPattern = regexp.MustCompile(`^[0-9]{1,5}$`)
	// localDomainPattern is local-host.sh's valid_host: a DNS name or an IPv4
	// address, no leading dot or dash, no trailing dot (and no "..", checked apart).
	localDomainPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9-])?$`)
	localBackupFile    = regexp.MustCompile(`^([0-9]{8}T[0-9]{6}Z)\.(dump|sha256)$`)
	// scriptSafePath is what can sit inside the facts script's double quotes.
	scriptSafePath = regexp.MustCompile("^/[^\"$`\\\\\x00-\x1f\x7f]*$")
)

// localGOOS is the platform --local runs on; tests replace it.
var localGOOS = runtime.GOOS

// trustRoots is the pool the trust check verifies against; nil is this
// machine's own store. Tests replace it.
var trustRoots *x509.CertPool

// localTarget validates the --local flags and resolves the folder.
func localTarget(t cloudTarget, bucket string) (cloudTarget, error) {
	if !t.local {
		return t, errors.New("--local-dir goes with --local")
	}
	if localGOOS == "windows" {
		return t, errors.New("--local runs its check with sh, as deploy/local-host.sh does: on Windows, run it inside WSL")
	}
	other := t
	other.local, other.localDir = false, ""
	if other.set() || bucket != "" {
		return t, errors.New("--local checks the server on this machine: it cannot be combined with --ssh, --vm, --instance, --project, --zone, --region, --profile, --account or --bucket")
	}
	dir := t.localDir
	if dir == "" {
		dir = os.Getenv("CLAWBRIDGE_LOCAL_DIR")
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return t, errors.New("cannot find the home folder; pass --local-dir DIR")
		}
		dir = filepath.Join(home, ".clawbridge-server")
	}
	if !filepath.IsAbs(dir) {
		return t, errors.New("--local-dir must be an absolute path")
	}
	t.localDir = filepath.Clean(dir)
	return t, nil
}

func localEnvPath(dir string) string { return filepath.Join(dir, localEnvName) }

func localEnvAdvice(dir string) string {
	return localEnvPath(dir) + " is missing or unreadable; start the server with deploy/local-host.sh up --owner EMAIL (or pass --local-dir)"
}

// localNoServer is the result when --local has no --server and the env file
// cannot name the server: missing, or present without a usable address.
func localNoServer(check, dir string) cloudResult {
	if _, ok := localEnv(dir); !ok {
		return failed(check, "env_missing", localEnvAdvice(dir))
	}
	return failed(check, "setting_invalid", localEnvPath(dir)+" does not name the server: it needs CLAWBRIDGE_LOCAL_SHAPE=solo with CLAWBRIDGE_LOCAL_PORT, or CLAWBRIDGE_LOCAL_SHAPE=lan with CLAWBRIDGE_DOMAIN; run deploy/local-host.sh up, or pass --server")
}

// localFactsScript is the facts script against the local env file.
func localFactsScript(dir string) (string, error) {
	path := localEnvPath(dir)
	if !scriptSafePath.MatchString(path) {
		return "", errors.New("--local-dir has a character the check cannot pass to the shell")
	}
	return factsScriptWith(`"`+path+`"`, false), nil
}

// localSettings are the env file's keys that name the server's address and
// shape. Of the certificate, only whether one is set: compose.local.yaml then
// runs caddy-cert in place of caddy.
type localSettings struct {
	shape, port, domain string
	ownCert             bool
}

// localEnv reads those keys, the last line of each as local-host.sh does.
// Nothing else is kept.
func localEnv(dir string) (s localSettings, ok bool) {
	raw, err := os.ReadFile(localEnvPath(dir))
	if err != nil {
		return s, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, found := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !found || strings.HasPrefix(strings.TrimSpace(name), "#") {
			continue
		}
		switch name {
		case "CLAWBRIDGE_LOCAL_SHAPE":
			s.shape = value
		case "CLAWBRIDGE_LOCAL_PORT":
			s.port = value
		case "CLAWBRIDGE_DOMAIN":
			s.domain = value
		case "CLAWBRIDGE_TLS_CERT":
			s.ownCert = value != ""
		}
	}
	return s, true
}

// localOrigin is the server's URL from the env file: http://127.0.0.1:PORT in
// shape A (solo), https://DOMAIN in shape B (lan); "" when it can't say.
func localOrigin(dir string) string {
	s, ok := localEnv(dir)
	switch {
	case !ok:
	case s.shape == "solo" && localPortPattern.MatchString(s.port):
		return "http://127.0.0.1:" + s.port
	case s.shape == "lan" && localDomainPattern.MatchString(s.domain) && !strings.Contains(s.domain, ".."):
		return "https://" + s.domain
	}
	return ""
}

// checkLocalServer follows a healthy /healthz with the env file's settings,
// the containers and the running sign-in methods. The local stack signs in
// with device keys only (compose.local.yaml passes --auth device).
func (p *cloudProbe) checkLocalServer(ctx context.Context) cloudResult {
	f, err := p.facts(ctx)
	if err != nil {
		return p.remoteFailure("server", err)
	}
	settings, _ := localEnv(p.target.localDir)
	if r := checkLocalSettings(f, p.target.localDir); r.Status != "pass" {
		return r
	}
	if r := checkLocalServices(f, settings); r.Status != "pass" {
		return r
	}
	return p.checkAuthMode(ctx, "device")
}

// checkLocalSettings: the compose file's required entries are set once each.
// The Google sign-in settings do not apply: the local stack is device-only.
func checkLocalSettings(f remoteFacts, dir string) cloudResult {
	if f.envMissing {
		return failed("server", "env_missing", localEnvAdvice(dir))
	}
	var missing, twice []string
	for _, k := range requiredSettings {
		switch n := f.counts[k]; {
		case n == 0:
			missing = append(missing, k)
		case n > 1:
			twice = append(twice, k)
		}
	}
	if len(missing) > 0 {
		return failed("server", "setting_missing", "set in "+localEnvPath(dir)+": "+strings.Join(missing, ", "))
	}
	if len(twice) > 0 {
		return failed("server", "setting_duplicate", "set once only in "+localEnvPath(dir)+": "+strings.Join(twice, ", "))
	}
	return cloudResult{Check: "server", Status: "pass"}
}

// checkLocalServices: db and api run, and in shape B (lan) caddy too, or
// caddy-cert when the env file names your own certificate.
func checkLocalServices(f remoteFacts, s localSettings) cloudResult {
	want := []string{"api", "db"}
	switch {
	case s.shape == "lan" && s.ownCert:
		want = []string{"api", "caddy-cert", "db"}
	case s.shape == "lan":
		want = []string{"api", "caddy", "db"}
	}
	r := checkServicesOf(f, want)
	if r.Status != "pass" {
		r.Advice = strings.Replace(r.Advice, "run docker compose up -d and check its logs", "run deploy/local-host.sh up, and see deploy/local-host.sh status", 1)
	}
	return r
}

// checkTrust: a TLS handshake with this machine's trust store. A server on
// plain http to loopback has no certificate to trust.
func checkTrust(ctx context.Context, p *cloudProbe) cloudResult {
	if p.server == "" {
		return localNoServer("trust", p.target.localDir)
	}
	u, ok := serverURL(p.server)
	if !ok {
		return failed("trust", "invalid_url", "use https://YOUR-DOMAIN (plain http only for localhost)")
	}
	if u.Scheme != "https" {
		return cloudResult{Check: "trust", Status: "pass", Reason: "not_applicable", Advice: "plain http to this machine has no certificate"}
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: u.Hostname(), RootCAs: trustRoots, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err == nil {
		conn.Close()
		return cloudResult{Check: "trust", Status: "pass"}
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return failed("trust", "untrusted_ca", "this machine does not trust the server's certificate; run deploy/local-host.sh trust, then follow \"Trusting the certificate\" in docs/operations/local-host.md")
	}
	reason := transportReason(err)
	return failed("trust", reason, transportAdvice[reason])
}

// checkLocalBackups: the newest complete dump and checksum pair in
// DIR/backups is under 26 hours old. A missing or stale backup warns: the
// server runs without one until the person schedules it.
func checkLocalBackups(dir string) cloudResult {
	advice := "run deploy/local-host.sh backup, and see \"Scheduling it\" in docs/operations/local-host.md"
	warn := func(reason, why string) cloudResult {
		return cloudResult{Check: "backups", Status: "warn", Reason: reason, Advice: why + "; " + advice}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "backups"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed("backups", "backups_unreadable", "cannot read "+filepath.Join(dir, "backups")+"; check its permissions")
	}
	kinds := map[string]map[string]bool{}
	for _, e := range entries {
		if m := localBackupFile.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			if kinds[m[1]] == nil {
				kinds[m[1]] = map[string]bool{}
			}
			kinds[m[1]][m[2]] = true
		}
	}
	var complete []string
	for stamp, k := range kinds {
		if k["dump"] && k["sha256"] {
			complete = append(complete, stamp)
		}
	}
	if len(complete) == 0 {
		why := "no backup yet"
		if len(kinds) > 0 {
			why = "no complete backup (a dump needs its .sha256)"
		}
		return warn("no_backup", why)
	}
	slices.Sort(complete)
	newest, err := time.Parse("20060102T150405Z", complete[len(complete)-1])
	if err != nil {
		return warn("no_backup", "no backup yet")
	}
	if age := cloudNow().Sub(newest); age > maxBackupAge {
		return warn("backup_stale", fmt.Sprintf("the newest backup is %s old", age.Truncate(time.Hour)))
	}
	return cloudResult{Check: "backups", Status: "pass"}
}
