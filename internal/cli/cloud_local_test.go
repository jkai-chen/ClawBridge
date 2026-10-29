package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const localSentinel = "secret-sentinel-value"

// localDir is a fake local server folder with an env file of the given shape.
func localDir(t *testing.T, shape, port string) string {
	t.Helper()
	dir := t.TempDir()
	body := "COMPOSE_PROJECT_NAME=clawbridge-local\nCLAWBRIDGE_LOCAL_SHAPE=" + shape + "\nCLAWBRIDGE_LOCAL_PORT=" + port +
		"\nCLAWBRIDGE_BOOTSTRAP_OWNER=owner@example.test\nCLAWBRIDGE_DOMAIN=clawbridge.lan\nPOSTGRES_PASSWORD=" + localSentinel +
		"\nCLAWBRIDGE_LINEAR_APP_TOKEN_KEY=" + localSentinel + "\n"
	if err := os.WriteFile(filepath.Join(dir, localEnvName), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unixHost runs --local as on Linux or macOS. Native Windows refuses --local
// (TestLocalRefusedOnNativeWindows), so these tests skip there.
func unixHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("--local is refused on native Windows")
	}
	old := localGOOS
	localGOOS = "linux"
	t.Cleanup(func() { localGOOS = old })
}

func localFacts(shape string) remoteFacts {
	f := allSet()
	for _, k := range googleSettings {
		delete(f.counts, k)
	}
	if shape != "lan" {
		delete(f.services, "caddy")
	}
	return f
}

func deviceServer(t *testing.T) string {
	return configServer(t, 200, `{"status":"ok","version":"1.2.3"}`, `{"auth_methods":["device"]}`)
}

func TestLocalTargetBuildsALocalShellNeverSSH(t *testing.T) {
	isolated(t)
	unixHost(t)
	withVersion(t, "1.2.3")
	dir := localDir(t, "solo", "8443")
	runs := stubRemote(t, factsOutput(localFacts("solo")), nil)
	got, err := cloudCheck(t, "server", "--server", deviceServer(t), "--local", "--local-dir", dir)
	if err != nil || !reflect.DeepEqual(got, []cloudResult{{"server", "pass", "", ""}}) {
		t.Fatalf("%+v %v", got, err)
	}
	script, _ := localFactsScript(dir)
	want := []string{"sh", "-c", script}
	if len(*runs) != 1 || !reflect.DeepEqual((*runs)[0].argv, want) {
		t.Fatalf("runs: %+v", *runs)
	}
	if !strings.Contains(script, `f="`+dir+`/clawbridge.env"`) || strings.Contains(script, "findmnt") {
		t.Fatalf("script: %s", script)
	}
	// The remote argv and script are unchanged.
	if remote, _ := (cloudTarget{host: "admin@bridge.example.test"}).argv("x"); !reflect.DeepEqual(remote, []string{"ssh", "-T", "admin@bridge.example.test", "x"}) {
		t.Fatalf("%v", remote)
	}
	if factsScript() != factsScriptFor(serverEnvFile) || !strings.Contains(factsScript(), "findmnt") || !strings.HasPrefix(factsScript(), "f="+serverEnvFile+"\n") {
		t.Fatal("remote facts script changed")
	}
}

func TestLocalFactsScriptRunsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("--local runs sh, which native Windows lacks")
	}
	dir := t.TempDir()
	body := "POSTGRES_PASSWORD=" + localSentinel + "\nCLAWBRIDGE_DOMAIN=clawbridge.lan\nCLAWBRIDGE_BOOTSTRAP_OWNER=a@example.test\nCOMPOSE_PROJECT_NAME=p\n"
	if err := os.WriteFile(filepath.Join(dir, localEnvName), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := localFactsScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A PATH with no docker: the script still prints the counts and no value.
	t.Setenv("PATH", "/usr/bin:/bin")
	out, err := remoteOutput(context.Background(), []string{"sh", "-c", script}, nil)
	if err != nil || strings.Contains(string(out), localSentinel) || !strings.Contains(string(out), "count POSTGRES_PASSWORD 1") {
		t.Fatalf("%q %v", out, err)
	}
	for _, bad := range []string{`/a"b`, "/a$b", "/a`b", `/a\b`, "/a\nb", "/a\x7fb"} {
		if _, err = localFactsScript(dir + bad); err == nil {
			t.Errorf("%q in the folder was accepted", bad)
		}
	}
	if _, err = localFactsScript(dir + "/with space"); err != nil {
		t.Errorf("a space was refused: %v", err)
	}
}

func TestLocalServicesFollowTheShape(t *testing.T) {
	isolated(t)
	unixHost(t)
	withVersion(t, "1.2.3")
	url := deviceServer(t)
	// Shape A has no caddy: its absence is fine.
	dir := localDir(t, "solo", "8443")
	stubRemote(t, factsOutput(localFacts("solo")), nil)
	if got, _ := cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir); got[0].Status != "pass" {
		t.Fatalf("%+v", got)
	}
	// Shape B needs it.
	dir = localDir(t, "lan", "8443")
	stubRemote(t, factsOutput(localFacts("solo")), nil)
	got, _ := cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir)
	if got[0].Reason != "service_down" || !strings.Contains(got[0].Advice, "caddy") || !strings.Contains(got[0].Advice, "local-host.sh") {
		t.Fatalf("%+v", got)
	}
	stubRemote(t, factsOutput(localFacts("lan")), nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir); got[0].Status != "pass" {
		t.Fatalf("%+v", got)
	}
	// Shape B with your own certificate (lan-cert) runs caddy-cert, not caddy.
	certDir := localDir(t, "lan", "8443")
	env := filepath.Join(certDir, localEnvName)
	body, _ := os.ReadFile(env)
	if err := os.WriteFile(env, append(body, "CLAWBRIDGE_TLS_CERT=/home/me/certs/cert.pem\n"...), 0600); err != nil {
		t.Fatal(err)
	}
	ownCert := localFacts("lan")
	ownCert.services["caddy-cert"] = ownCert.services["caddy"]
	delete(ownCert.services, "caddy")
	stubRemote(t, factsOutput(ownCert), nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", certDir); got[0].Status != "pass" {
		t.Fatalf("lan-cert: %+v", got)
	}
	stubRemote(t, factsOutput(localFacts("lan")), nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", certDir); got[0].Reason != "service_down" || !strings.Contains(got[0].Advice, "caddy-cert") {
		t.Fatalf("lan-cert without caddy-cert: %+v", got)
	}
	// A stopped database; a missing setting; Google's settings are not needed.
	down := localFacts("lan")
	down.services["db"] = []string{"exited Exited (1) 1 minute ago"}
	stubRemote(t, factsOutput(down), nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir); got[0].Reason != "service_down" {
		t.Fatalf("%+v", got)
	}
	missing := localFacts("lan")
	missing.counts["POSTGRES_PASSWORD"] = 0
	stubRemote(t, factsOutput(missing), nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir); got[0].Reason != "setting_missing" || !strings.Contains(got[0].Advice, dir) {
		t.Fatalf("%+v", got)
	}
	stubRemote(t, "env missing\n", nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir); got[0].Reason != "env_missing" {
		t.Fatalf("%+v", got)
	}
	stubRemote(t, "", os.ErrPermission)
	if got, _ = cloudCheck(t, "server", "--server", url, "--local", "--local-dir", dir); got[0].Reason != "local_unreadable" {
		t.Fatalf("%+v", got)
	}
	// The running server must sign in with device keys.
	stubRemote(t, factsOutput(localFacts("lan")), nil)
	google := healthServer(t, 200, `{"status":"ok","version":"1.2.3"}`)
	if got, _ = cloudCheck(t, "server", "--server", google, "--local", "--local-dir", dir); got[0].Reason != "auth_mismatch" {
		t.Fatalf("%+v", got)
	}
}

func TestLocalServerURLFromTheEnvFile(t *testing.T) {
	isolated(t)
	unixHost(t)
	withVersion(t, "1.2.3")
	if got := localOrigin(localDir(t, "solo", "8443")); got != "http://127.0.0.1:8443" {
		t.Fatal(got)
	}
	if got := localOrigin(localDir(t, "lan", "8443")); got != "https://clawbridge.lan" {
		t.Fatal(got)
	}
	if got := localOrigin(t.TempDir()); got != "" {
		t.Fatal(got)
	}
	bad := localDir(t, "other", "8443")
	if got := localOrigin(bad); got != "" {
		t.Fatal(got)
	}
	// The domain follows local-host.sh's valid_host: a DNS name or IPv4.
	for domain, want := range map[string]string{
		"192.168.1.20": "https://192.168.1.20", "box-1.lan": "https://box-1.lan",
		"[::1]": "", ".lan": "", "-lan": "", "lan.": "", "a..lan": "", "a b": "",
	} {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, localEnvName), []byte("CLAWBRIDGE_LOCAL_SHAPE=lan\nCLAWBRIDGE_DOMAIN="+domain+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := localOrigin(d); got != want {
			t.Errorf("%q: %q", domain, got)
		}
	}
	// End to end: no --server, shape A, the port from the env file; no secret in the output.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"1.2.3"}`))
	}))
	t.Cleanup(srv.Close)
	dir := localDir(t, "solo", strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	var out, errOut bytes.Buffer
	err := Run(context.Background(), []string{"cloud", "check", "--json", "version", "--local", "--local-dir", dir}, &out, &errOut)
	if err != nil || !strings.Contains(out.String(), `"status":"pass"`) || strings.Contains(out.String()+errOut.String(), localSentinel) {
		t.Fatalf("%v %s %s", err, out.String(), errOut.String())
	}
	// The default folder comes from CLAWBRIDGE_LOCAL_DIR.
	t.Setenv("CLAWBRIDGE_LOCAL_DIR", dir)
	out.Reset()
	if err = Run(context.Background(), []string{"cloud", "check", "--json", "version", "--local"}, &out, &errOut); err != nil || !strings.Contains(out.String(), `"pass"`) {
		t.Fatalf("%v %s", err, out.String())
	}
	// No env file and no --server: nothing to check against, for trust too.
	empty := t.TempDir()
	for _, check := range []string{"server", "trust"} {
		got, _ := cloudCheck(t, check, "--local", "--local-dir", empty)
		if got[0].Reason != "env_missing" || !strings.Contains(got[0].Advice, "local-host.sh up") {
			t.Fatalf("%s: %+v", check, got)
		}
		// An env file that names no usable address says which keys.
		got, _ = cloudCheck(t, check, "--local", "--local-dir", bad)
		if got[0].Reason != "setting_invalid" || !strings.Contains(got[0].Advice, "CLAWBRIDGE_LOCAL_SHAPE") || strings.Contains(got[0].Advice, localSentinel) {
			t.Fatalf("%s: %+v", check, got)
		}
	}
}

func TestLocalSecretsStayOutOfTheOutput(t *testing.T) {
	isolated(t)
	unixHost(t)
	withVersion(t, "1.2.3")
	dir := localDir(t, "solo", "8443")
	stubRemote(t, factsOutput(localFacts("solo")), nil)
	var out, errOut bytes.Buffer
	_ = Run(context.Background(), []string{"cloud", "check", "server", "--server", deviceServer(t), "--local", "--local-dir", dir}, &out, &errOut)
	if strings.Contains(out.String()+errOut.String(), localSentinel) {
		t.Fatal("a secret reached the output")
	}
}

func TestLocalTrust(t *testing.T) {
	isolated(t)
	unixHost(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	// The test server's certificate is not in the system store.
	got, err := cloudCheck(t, "trust", "--server", srv.URL, "--local", "--local-dir", dir)
	if err == nil || got[0].Reason != "untrusted_ca" || !strings.Contains(got[0].Advice, "deploy/local-host.sh trust") || !strings.Contains(got[0].Advice, "Trusting the certificate") {
		t.Fatalf("%+v %v", got, err)
	}
	// With its root in the pool it passes.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	old := trustRoots
	trustRoots = pool
	t.Cleanup(func() { trustRoots = old })
	if got, err = cloudCheck(t, "trust", "--server", srv.URL, "--local", "--local-dir", dir); err != nil || !reflect.DeepEqual(got, []cloudResult{{"trust", "pass", "", ""}}) {
		t.Fatalf("%+v %v", got, err)
	}
	// Plain http to loopback has no certificate.
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	if got, err = cloudCheck(t, "trust", "--server", plain.URL, "--local", "--local-dir", dir); err != nil || got[0].Status != "pass" || got[0].Reason != "not_applicable" {
		t.Fatalf("%+v %v", got, err)
	}
	// Nothing listens: unreachable, not a trust problem.
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if got, _ = cloudCheck(t, "trust", "--server", url, "--local", "--local-dir", dir); got[0].Reason != "unreachable" {
		t.Fatalf("%+v", got)
	}
	// An untrusted server also fails the server check, pointing at trust.
	trustRoots = nil
	if got, _ = cloudCheck(t, "server", "--server", srv.URL, "--local", "--local-dir", dir); got[0].Reason != "tls" || !strings.Contains(got[0].Advice, "trust") {
		t.Fatalf("%+v", got)
	}
}

func TestTrustIsLocalOnly(t *testing.T) {
	isolated(t)
	if err := Run(context.Background(), []string{"cloud", "check", "trust", "--server", "https://clawbridge.example.test"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--local") {
		t.Fatalf("%v", err)
	}
	// "all" leaves it out without --local.
	var out bytes.Buffer
	_ = Run(context.Background(), []string{"cloud", "check", "--json", "all", "--server", "https://127.0.0.1:1"}, &out, &bytes.Buffer{})
	if strings.Contains(out.String(), `"trust"`) {
		t.Fatal(out.String())
	}
}

func TestLocalBackups(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := cloudNow
	cloudNow = func() time.Time { return now }
	t.Cleanup(func() { cloudNow = old })
	put := func(dir string, names ...string) {
		if err := os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, "backups", n), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	reason := func(dir string) (string, string) {
		r := checkLocalBackups(dir)
		return r.Status, r.Reason
	}
	fresh, stale, none, partial, noDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	put(fresh, "20261029T020000Z.dump", "20261029T020000Z.sha256")
	put(stale, "20261027T020000Z.dump", "20261027T020000Z.sha256")
	put(none)
	put(partial, "20261029T020000Z.dump")
	for dir, want := range map[string][2]string{
		fresh: {"pass", ""}, stale: {"warn", "backup_stale"}, none: {"warn", "no_backup"},
		partial: {"warn", "no_backup"}, noDir: {"warn", "no_backup"},
	} {
		if s, r := reason(dir); s != want[0] || r != want[1] {
			t.Errorf("%s: %s %s, want %v", dir, s, r, want)
		}
	}
	if r := checkLocalBackups(stale); !strings.Contains(r.Advice, "deploy/local-host.sh backup") || !strings.Contains(r.Advice, "Scheduling it") {
		t.Fatalf("%+v", r)
	}
	// An older complete pair beats a newer dump with no checksum.
	put(partial, "20261029T040000Z.dump", "20261028T200000Z.dump", "20261028T200000Z.sha256")
	if s, _ := reason(partial); s != "pass" {
		t.Fatal(s)
	}
	// Through the command: no --bucket needed.
	isolated(t)
	unixHost(t)
	unixHost(t)
	got, err := cloudCheck(t, "backups", "--local", "--local-dir", stale)
	if err != nil || got[0].Status != "warn" || got[0].Reason != "backup_stale" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLocalInfraIsNotApplicable(t *testing.T) {
	isolated(t)
	unixHost(t)
	runs := stubRemote(t, "", nil)
	got, err := cloudCheck(t, "infra", "--local", "--local-dir", t.TempDir())
	if err != nil || !reflect.DeepEqual(got, []cloudResult{{"infra", "pass", "not_applicable", got[0].Advice}}) || got[0].Advice == "" || len(*runs) != 0 {
		t.Fatalf("%+v %v %v", got, err, *runs)
	}
}

func TestLocalRefusesOtherTargets(t *testing.T) {
	isolated(t)
	unixHost(t)
	runs := stubRemote(t, "", nil)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"--local", "--ssh", "host"},
		{"--local", "--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b"},
		{"--local", "--instance", "i-0123456789abcdef0", "--region", "eu-west-1"},
		{"--local", "--bucket", "example-backups"},
		{"--local", "--project", "example-project"},
		{"--local-dir", dir},
		{"--local", "--local-dir", "relative"},
	} {
		err := Run(context.Background(), append([]string{"cloud", "check"}, args...), &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || len(*runs) != 0 {
			t.Errorf("%v: %v %v", args, err, *runs)
		}
	}
}

func TestLocalJSONOutput(t *testing.T) {
	isolated(t)
	unixHost(t)
	withVersion(t, "1.2.3")
	dir := localDir(t, "solo", "8443")
	stubRemote(t, factsOutput(localFacts("solo")), nil)
	// All checks, through JSON: infra and trust are not applicable, the rest
	// answer without a cloud profile.
	got, err := cloudCheck(t, "--server", deviceServer(t), "--local", "--local-dir", dir)
	if err == nil {
		t.Fatal("workspace needs a cloud profile, so the run must fail")
	}
	names := []string{}
	for _, r := range got {
		names = append(names, r.Check+":"+r.Status+":"+r.Reason)
	}
	want := []string{"infra:pass:not_applicable", "server:pass:", "trust:pass:not_applicable", "version:pass:", "workspace:fail:no_config", "linear-app:fail:no_config", "backups:warn:no_backup"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("%v", names)
	}
}

func TestLocalRefusedOnNativeWindows(t *testing.T) {
	isolated(t)
	old := localGOOS
	localGOOS = "windows"
	t.Cleanup(func() { localGOOS = old })
	runs := stubRemote(t, "", nil)
	err := Run(context.Background(), []string{"cloud", "check", "--local", "--local-dir", t.TempDir()}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "WSL") || len(*runs) != 0 {
		t.Fatalf("%v %v", err, *runs)
	}
}
