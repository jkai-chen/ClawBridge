package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/server"
)

// shellSecrets are set in the shell that runs local-host.sh. None may reach a
// docker call: the env file decides.
var shellSecrets = []string{
	"POSTGRES_PASSWORD=from-the-shell",
	"CLAWBRIDGE_PUBLIC_ORIGIN=https://from-the-shell.example.test",
	"CLAWBRIDGE_LINEAR_APP_CLIENT_ID=from-the-shell",
	"CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=from-the-shell",
	"CLAWBRIDGE_BOOTSTRAP_OWNER=from-the-shell@example.test",
}

type fakeRun struct {
	out   string
	code  int
	calls string // one line per docker call: its arguments
	bin   string // call-N.argv (one argument per line) and call-N.env
}

// localHostRun runs deploy/local-host.sh with fake docker and curl first on
// PATH: nothing reaches a real Docker. It returns the output, the exit code
// and the docker calls made.
func localHostRun(t *testing.T, dir string, args ...string) (string, int, string) {
	t.Helper()
	r := localHostFake(t, dir, "", nil, args...)
	return r.out, r.code, r.calls
}

// localHostFake is localHostRun with the fake psql's answer (the owner of the
// workspace asked about; empty for none), and every call's argv and env kept.
// It fails the test if a shell secret reached any docker call.
func localHostFake(t *testing.T, dir, psqlAnswer string, extraFakes map[string]string, args ...string) fakeRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local-host.sh is a POSIX shell script")
	}
	fakeBin := t.TempDir()
	q := shellQuote(fakeBin)
	if psqlAnswer != "" {
		if err := os.WriteFile(filepath.Join(fakeBin, "psql.answer"), []byte(psqlAnswer+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fake := map[string]string{
		// compose version, info: present and running. volume inspect: no
		// volume yet (a first start). psql: psql.answer. Everything else
		// succeeds.
		"docker": "#!/bin/sh\nb=" + q + "\n" +
			"n=$(($(cat \"$b/count\" 2>/dev/null || echo 0) + 1)); echo \"$n\" >\"$b/count\"\n" +
			"printf '%s\\n' \"$*\" >>\"$b/docker.log\"\n" +
			"printf '%s\\n' \"$@\" >\"$b/call-$n.argv\"\n" +
			"env >\"$b/call-$n.env\"\n" +
			"case \"$1 $2\" in \"volume inspect\") exit 1 ;; esac\n" +
			"case \"$*\" in \"compose version --short\") if [ -f \"$b/compose.version\" ]; then cat \"$b/compose.version\"; else echo 2.30.0; fi; exit 0 ;; esac\n" +
			"case \"$*\" in *' psql '*) [ ! -f \"$b/psql.answer\" ] || cat \"$b/psql.answer\" ;; esac\n" +
			"exit 0\n",
		"curl": "#!/bin/sh\nprintf '%s\\n' '{\"status\":\"ok\",\"version\":\"test\"}'\n",
	}
	for name, body := range extraFakes {
		fake[name] = body
	}
	for name, body := range fake {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", append([]string{filepath.Join(repositoryRoot(t), "deploy", "local-host.sh")}, args...)...)
	cmd.Env = append([]string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(), "LANG=C", "LC_ALL=C",
		"CLAWBRIDGE_LOCAL_DIR=" + dir,
		"CLAWBRIDGE_LOCAL_PROJECT=cb-localhost-fake",
	}, shellSecrets...)
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	envs, _ := filepath.Glob(filepath.Join(fakeBin, "call-*.env"))
	for _, path := range envs {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "from-the-shell") {
			t.Fatalf("%q: a shell variable reached docker (%s):\n%s", args, filepath.Base(path), raw)
		}
	}
	ran, _ := os.ReadFile(filepath.Join(fakeBin, "docker.log"))
	return fakeRun{out: string(out), code: code, calls: string(ran), bin: fakeBin}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("env file line without =: %q", line)
		}
		if _, dup := values[k]; dup {
			t.Fatalf("env file sets %s twice", k)
		}
		values[k] = v
	}
	return values
}

// Every mistake is a usage error (exit 2) found before Docker is touched.
func TestLocalHostScriptArguments(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"up"},
		{"up", "--owner"},
		{"up", "--owner", "not-an-email"},
		{"up", "--owner", "owner@example.test", "--workspace", "Bad Name"},
		{"up", "--owner", "owner@example.test", "--workspace", "Upper"},
		{"up", "--owner", "owner@example.test", "--port", "80"},
		{"up", "--owner", "owner@example.test", "--port", "08443"},
		{"up", "--owner", "owner@example.test", "--bind", "192.0.2.10"},
		{"up", "--owner", "owner@example.test", "--lan", "bad host"},
		{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--bind", "256.0.0.1"},
		{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--cert", "/no/such/cert.pem", "--key", "/no/such/key.pem"},
		{"status", "extra"},
		{"uninstall", "--yes"},
		{"uninstall", "--force"},
		{"invite-owner", "--workspace", "Bad"},
	} {
		dir := filepath.Join(t.TempDir(), "server")
		out, code, ran := localHostRun(t, dir, args...)
		if code != 2 || ran != "" {
			t.Errorf("%q: exit %d, docker calls %q\n%s", args, code, ran, out)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%q created %s", args, dir)
		}
	}
	// Each of these is refused with its own message.
	for msg, cases := range map[string][][]string{
		"--cert and --key go together": {
			{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--cert", "/etc/hosts"},
			{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--key", "/etc/hosts"},
		},
		"name the LAN address to publish on (for example 192.0.2.10)": {
			{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--bind", "0.0.0.0"},
			{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--bind", "::"},
			{"up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--bind", ""},
		},
	} {
		for _, args := range cases {
			dir := filepath.Join(t.TempDir(), "server")
			out, code, ran := localHostRun(t, dir, args...)
			if code != 2 || ran != "" || !strings.Contains(out, msg) {
				t.Errorf("%q: exit %d, docker calls %q, want %q\n%s", args, code, ran, msg, out)
			}
		}
	}

	// A command that needs a server says how to make one.
	out, code, _ := localHostRun(t, filepath.Join(t.TempDir(), "none"), "status")
	if code != 1 || !strings.Contains(out, "up --owner EMAIL") {
		t.Fatalf("status with no server: %d %s", code, out)
	}
}

// The first up writes the env file once, private, with fresh keys; it never
// prints them, and a later up neither rewrites it nor accepts a contradiction.
func TestLocalHostScriptEnvFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	out, code, ran := localHostRun(t, dir, "up", "--owner", "owner@example.test", "--port", "18443")
	if code != 0 {
		t.Fatalf("up: %d\n%s", code, out)
	}
	envPath := filepath.Join(dir, "clawbridge.env")
	info, err := os.Stat(envPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %v %v", info, err)
	}
	if info, err = os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("folder: %v %v", info, err)
	}
	env := readEnvFile(t, envPath)
	want := map[string]string{
		"COMPOSE_PROJECT_NAME": "cb-localhost-fake", "CLAWBRIDGE_LOCAL_SHAPE": "solo",
		"CLAWBRIDGE_LOCAL_PORT": "18443", "CLAWBRIDGE_BOOTSTRAP_OWNER": "owner@example.test",
		"CLAWBRIDGE_DOMAIN": "127.0.0.1",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s=%q, want %q", k, env[k], v)
		}
	}
	if len(env["POSTGRES_PASSWORD"]) != 64 || len(env["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"]) != 44 {
		t.Fatalf("keys: %d %d", len(env["POSTGRES_PASSWORD"]), len(env["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"]))
	}
	for _, secret := range []string{env["POSTGRES_PASSWORD"], env["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"]} {
		if strings.Contains(out, secret) || strings.Contains(ran, secret) {
			t.Fatal("a secret was printed or passed on a command line")
		}
	}
	if !strings.Contains(out, "clawbridge join --server http://127.0.0.1:18443 --email owner@example.test") {
		t.Fatalf("no join command:\n%s", out)
	}
	if !strings.Contains(out, "invite-owner") {
		t.Fatalf("a non-terminal run must say how to get the code:\n%s", out)
	}
	if !strings.Contains(ran, "--project-name cb-localhost-fake") || !strings.Contains(ran, "up -d --build") || strings.Contains(ran, "bootstrap-invite") {
		t.Fatalf("docker calls:\n%s", ran)
	}
	before, _ := os.ReadFile(envPath)

	if out, code, _ = localHostRun(t, dir, "up"); code != 0 {
		t.Fatalf("second up: %d\n%s", code, out)
	}
	for _, args := range [][]string{
		{"up", "--owner", "someone@example.test"},
		{"up", "--port", "18444"},
		{"up", "--lan", "clawbridge.lan"},
		{"trust"},
	} {
		if out, code, _ = localHostRun(t, dir, args...); code != 1 {
			t.Errorf("%q on a solo server: %d\n%s", args, code, out)
		}
	}
	after, _ := os.ReadFile(envPath)
	if !bytes.Equal(before, after) {
		t.Fatal("the env file changed")
	}

	// A LAN server with a brought certificate: copied in, readable by Caddy.
	lan := filepath.Join(t.TempDir(), "server")
	src := t.TempDir()
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err = os.WriteFile(filepath.Join(src, name), []byte("synthetic "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code, _ = localHostRun(t, lan, "up", "--owner", "owner@example.test", "--lan", "clawbridge.lan",
		"--bind", "192.0.2.10", "--cert", filepath.Join(src, "cert.pem"), "--key", filepath.Join(src, "key.pem"))
	if code != 0 || !strings.Contains(out, "clawbridge join --server https://clawbridge.lan --email owner@example.test") {
		t.Fatalf("lan up: %d\n%s", code, out)
	}
	env = readEnvFile(t, filepath.Join(lan, "clawbridge.env"))
	if env["CLAWBRIDGE_LOCAL_SHAPE"] != "lan" || env["CLAWBRIDGE_PUBLIC_ORIGIN"] != "https://clawbridge.lan" ||
		env["CLAWBRIDGE_BIND_ADDR"] != "192.0.2.10" || env["CLAWBRIDGE_TLS_CERT"] != filepath.Join(lan, "tls", "cert.pem") {
		t.Fatalf("lan env: %v", env)
	}
	if info, err = os.Stat(filepath.Join(lan, "tls", "key.pem")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged key: %v %v", info, err)
	}
	if info, err = os.Stat(filepath.Join(lan, "tls")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("tls folder: %v %v", info, err)
	}
	if _, err = os.Stat(filepath.Join(lan, "tls", "cert.pem")); err != nil {
		t.Fatal(err)
	}
}

// localService is one service of `docker compose config --format json`.
type localService struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Entrypoint  []string          `json:"entrypoint"`
	Environment map[string]string `json:"environment"`
	Networks    map[string]any    `json:"networks"`
	ReadOnly    bool              `json:"read_only"`
	User        string            `json:"user"`
	Sysctls     map[string]string `json:"sysctls"`
	CapDrop     []string          `json:"cap_drop"`
	Ports       []struct {
		HostIP    string `json:"host_ip"`
		Target    int    `json:"target"`
		Published string `json:"published"`
	} `json:"ports"`
	Volumes []renderedVolumeMount `json:"volumes"`
}

type localRendered struct {
	Services map[string]localService `json:"services"`
	Volumes  map[string]any          `json:"volumes"`
}

// renderLocal renders compose.yaml with compose.local.yaml from an env file
// local-host.sh wrote, with the profile it would use, and nothing from the
// host's environment.
func renderLocal(t *testing.T, envFile string, extra []string, profiles ...string) localRendered {
	t.Helper()
	deploy := filepath.Join(repositoryRoot(t), "deploy")
	args := []string{"compose", "--project-name", "cb-localhost-render", "--project-directory", deploy, "--env-file", envFile,
		"-f", filepath.Join(deploy, "compose.yaml"), "-f", filepath.Join(deploy, "compose.local.yaml")}
	for _, p := range profiles {
		args = append(args, "--profile", p)
	}
	cmd := exec.Command("docker", append(args, "config", "--format", "json")...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	var doc localRendered
	if err = json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Both shapes render from the env files local-host.sh writes: every variable
// compose.yaml requires is satisfied, sign-in is device only, and only the
// chosen addresses are published.
func TestComposeLocalShapes(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	solo := filepath.Join(t.TempDir(), "server")
	if out, code, _ := localHostRun(t, solo, "up", "--owner", "owner@example.test", "--port", "18443"); code != 0 {
		t.Fatal(out)
	}
	soloEnv := filepath.Join(solo, "clawbridge.env")
	doc := renderLocal(t, soloEnv, nil)
	if names := sortedKeys(doc.Services); !reflect.DeepEqual(names, []string{"api", "db"}) {
		t.Fatalf("solo services: %v", names)
	}
	api := doc.Services["api"]
	wantCommand := []string{"serve", "--listen", "0.0.0.0:8080", "--auth", "device", "--public-origin", "http://127.0.0.1:18443", "--bootstrap-owner", "owner@example.test"}
	if !reflect.DeepEqual(api.Command, wantCommand) {
		t.Fatalf("solo api command: %q", api.Command)
	}
	settings, err := server.ParseServe(api.Command[1:], func(k string) string { return api.Environment[k] })
	if err != nil || fmt.Sprint(settings.Auth.Methods) != "[device]" || settings.Auth.PublicOrigin != "http://127.0.0.1:18443" {
		t.Fatalf("solo serve settings: %+v %v", settings.Auth, err)
	}
	if len(api.Ports) != 1 || api.Ports[0].HostIP != "127.0.0.1" || api.Ports[0].Published != "18443" || api.Ports[0].Target != 8080 {
		t.Fatalf("solo api ports: %+v", api.Ports)
	}
	if len(doc.Services["db"].Ports) != 0 || !api.ReadOnly || !reflect.DeepEqual(api.CapDrop, []string{"ALL"}) {
		t.Fatal("compose.yaml's hardening or db isolation was lost")
	}
	assertNetworks(t, api.Networks, "api", "backend", "egress")
	assertNetworks(t, doc.Services["db"].Networks, "db", "backend")
	if v := findVolumeMount(doc.Services["db"].Volumes, "/var/lib/postgresql/data"); v == nil || v.Type != "volume" || v.Source != "postgres-data" {
		t.Fatalf("db volume: %+v", v)
	}
	// S2: solo has no proxy, so it trusts no peer's forwarded headers.
	if got, ok := api.Environment["CLAWBRIDGE_TRUSTED_PROXIES"]; !ok || got != "" {
		t.Fatalf("solo trusts a proxy: %q %v", got, ok)
	}
	// The token key waits for the Linear app: serve refuses a key alone.
	if got, ok := api.Environment["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"]; !ok || got != "" {
		t.Fatalf("token key passed without the app: %q", got)
	}
	key := readEnvFile(t, soloEnv)["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"]
	withApp := renderLocal(t, soloEnv, []string{"CLAWBRIDGE_LINEAR_APP_CLIENT_ID=synthetic-client"})
	if withApp.Services["api"].Environment["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"] != key {
		t.Fatal("token key not passed once the app is set")
	}

	// B1: Caddy's internal CA, on the chosen address only.
	lan := filepath.Join(t.TempDir(), "server")
	if out, code, _ := localHostRun(t, lan, "up", "--owner", "owner@example.test", "--lan", "clawbridge.lan", "--bind", "192.0.2.10"); code != 0 {
		t.Fatal(out)
	}
	doc = renderLocal(t, filepath.Join(lan, "clawbridge.env"), nil, "lan")
	if names := sortedKeys(doc.Services); !reflect.DeepEqual(names, []string{"api", "caddy", "db"}) {
		t.Fatalf("lan services: %v", names)
	}
	api = doc.Services["api"]
	settings, err = server.ParseServe(api.Command[1:], func(k string) string { return api.Environment[k] })
	if err != nil || fmt.Sprint(settings.Auth.Methods) != "[device]" || settings.Auth.PublicOrigin != "https://clawbridge.lan" {
		t.Fatalf("lan serve settings: %+v %v", settings.Auth, err)
	}
	if got := api.Environment["CLAWBRIDGE_TRUSTED_PROXIES"]; got != "caddy-backend" {
		t.Fatalf("lan trusted proxies: %q", got)
	}
	caddy := doc.Services["caddy"]
	// A Caddyfile from CLAWBRIDGE_DOMAIN, with default_sni for clients that
	// send no server name (TestLocalHostCaddyWithoutSNI runs it).
	if !reflect.DeepEqual(caddy.Entrypoint, []string{"/bin/sh", "-c"}) || len(caddy.Command) != 1 ||
		!strings.Contains(caddy.Command[0], `default_sni %s`) || !strings.Contains(caddy.Command[0], "local_certs") ||
		caddy.Environment["CLAWBRIDGE_DOMAIN"] != "clawbridge.lan" {
		t.Fatalf("caddy: %q %q %v", caddy.Entrypoint, caddy.Command, caddy.Environment)
	}
	if len(caddy.Ports) != 1 || caddy.Ports[0].HostIP != "192.0.2.10" || caddy.Ports[0].Published != "443" {
		t.Fatalf("caddy ports: %+v", caddy.Ports)
	}
	if len(api.Ports) != 1 || api.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("lan api ports: %+v", api.Ports)
	}
	// The default bind is loopback: the LAN is an explicit choice.
	lanDefault := filepath.Join(t.TempDir(), "server")
	if out, code, _ := localHostRun(t, lanDefault, "up", "--owner", "owner@example.test", "--lan", "clawbridge.lan"); code != 0 {
		t.Fatal(out)
	}
	if p := renderLocal(t, filepath.Join(lanDefault, "clawbridge.env"), nil, "lan").Services["caddy"].Ports; len(p) != 1 || p[0].HostIP != "127.0.0.1" {
		t.Fatalf("default lan bind: %+v", p)
	}

	// B2/B3: a brought certificate, mounted read-only from the private folder.
	cert := filepath.Join(t.TempDir(), "server")
	src := t.TempDir()
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err = os.WriteFile(filepath.Join(src, name), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, code, _ := localHostRun(t, cert, "up", "--owner", "owner@example.test", "--lan", "clawbridge.lan",
		"--cert", filepath.Join(src, "cert.pem"), "--key", filepath.Join(src, "key.pem")); code != 0 {
		t.Fatal(out)
	}
	doc = renderLocal(t, filepath.Join(cert, "clawbridge.env"), nil, "lan-cert")
	if names := sortedKeys(doc.Services); !reflect.DeepEqual(names, []string{"api", "caddy-cert", "db"}) {
		t.Fatalf("lan-cert services: %v", names)
	}
	cc := doc.Services["caddy-cert"]
	// The api trusts caddy-backend; caddy-cert answers to that name on
	// backend only, and reaches the api by its backend-only name, so the
	// address it connects from is the one trusted.
	if got := doc.Services["api"].Environment["CLAWBRIDGE_TRUSTED_PROXIES"]; got != "caddy-backend" {
		t.Fatalf("lan-cert trusted proxies: %q", got)
	}
	if n, _ := cc.Networks["backend"].(map[string]any); fmt.Sprint(n["aliases"]) != "[caddy-backend]" {
		t.Fatalf("caddy-cert on backend: %v", cc.Networks["backend"])
	}
	if n, _ := cc.Networks["egress"].(map[string]any); n != nil && n["aliases"] != nil {
		t.Fatalf("caddy-cert on egress: %v", cc.Networks["egress"])
	}
	if len(cc.Command) != 1 || !strings.Contains(cc.Command[0], "reverse_proxy api-backend:8080") {
		t.Fatalf("caddy-cert must reach the api over backend: %q", cc.Command)
	}
	for target, source := range map[string]string{"/run/clawbridge-tls/cert.pem": filepath.Join(cert, "tls", "cert.pem"), "/run/clawbridge-tls/key.pem": filepath.Join(cert, "tls", "key.pem")} {
		if v := findVolumeMount(cc.Volumes, target); v == nil || v.Source != source {
			t.Fatalf("%s mount: %+v", target, v)
		}
	}
	if !cc.ReadOnly || !reflect.DeepEqual(cc.CapDrop, []string{"ALL"}) || cc.Environment["CLAWBRIDGE_DOMAIN"] != "clawbridge.lan" ||
		len(cc.Command) != 1 || !strings.Contains(cc.Command[0], `default_sni %s`) {
		t.Fatalf("caddy-cert: %+v", cc)
	}
	if len(cc.Ports) != 1 || cc.Ports[0].HostIP != "127.0.0.1" || cc.Ports[0].Published != "443" {
		t.Fatalf("caddy-cert ports: %+v", cc.Ports)
	}
	// It needs no CA state: no Caddy volume, so the key is its only secret.
	for _, v := range cc.Volumes {
		if v.Type == "volume" {
			t.Fatalf("caddy-cert mounts a volume: %+v", v)
		}
	}
	if cc.Sysctls["net.ipv4.ip_unprivileged_port_start"] != "443" || cc.User != "0:0" {
		t.Fatalf("caddy-cert user %q, sysctls %v", cc.User, cc.Sysctls)
	}
	// local-host.sh runs it as the person who owns the 0600 key.
	asUser := renderLocal(t, filepath.Join(cert, "clawbridge.env"), []string{"CLAWBRIDGE_LOCAL_UID=1000", "CLAWBRIDGE_LOCAL_GID=1000"}, "lan-cert")
	if u := asUser.Services["caddy-cert"].User; u != "1000:1000" {
		t.Fatalf("caddy-cert user with the uid passed: %q", u)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The real thing, opt-in (CLAWBRIDGE_TEST_DOCKER=1): local-host.sh up builds
// and starts shape A under a project of its own, and it answers on loopback
// only, signs in by device, backs up and uninstalls with its data.
func TestLocalHostDocker(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_DOCKER") != "1" {
		t.Skip("set CLAWBRIDGE_TEST_DOCKER=1 to build and run the local server in Docker")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local-host.sh is a POSIX shell script")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker is not running")
	}
	script := filepath.Join(repositoryRoot(t), "deploy", "local-host.sh")
	project := fmt.Sprintf("cb-localhost-test-%d", os.Getpid())
	t.Logf("compose project %s", project)
	port := freeLoopbackPort(t)
	dir := filepath.Join(t.TempDir(), "server")
	env := append(os.Environ(), "CLAWBRIDGE_LOCAL_DIR="+dir, "CLAWBRIDGE_LOCAL_PROJECT="+project)
	// Deadlines nest inside go test -timeout (15m in CI), so a stuck step
	// fails the test and t.Cleanup's teardown still runs: 11 minutes for
	// everything below, then at most 3 for the cleanup.
	testCtx, cancelTest := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancelTest()
	run := func(timeout time.Duration, args ...string) (string, error) {
		parent := testCtx
		if timeout == cleanupBudget {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...)
		cmd.Env = env
		cmd.Stdin = nil
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	uninstalled := false
	t.Cleanup(func() {
		if !uninstalled {
			if out, err := run(cleanupBudget, "uninstall", "--delete-data", "--yes"); err != nil {
				t.Logf("cleanup uninstall: %v\n%s", err, out)
			}
		}
		// Whatever the script left, by exact name.
		_ = exec.Command("docker", "compose", "-p", project, "down", "-v").Run()
		_ = exec.Command("docker", "image", "rm", project+"-api").Run()
	})

	out, err := run(8*time.Minute, "up", "--owner", "owner@example.test", "--port", fmt.Sprint(port))
	if err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !strings.Contains(out, "clawbridge join --server "+origin+" --email owner@example.test") {
		t.Fatalf("up printed no join command:\n%s", out)
	}
	envValues := readEnvFile(t, filepath.Join(dir, "clawbridge.env"))
	if strings.Contains(out, envValues["POSTGRES_PASSWORD"]) || strings.Contains(out, envValues["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"]) {
		t.Fatal("up printed a secret")
	}

	var health struct{ Status string }
	getJSON(t, origin+"/healthz", &health)
	if health.Status != "ok" {
		t.Fatalf("healthz: %+v", health)
	}
	var config struct {
		AuthMethods  []string `json:"auth_methods"`
		PublicOrigin string   `json:"public_origin"`
	}
	getJSON(t, origin+"/v1/config", &config)
	if !reflect.DeepEqual(config.AuthMethods, []string{"device"}) || config.PublicOrigin != origin {
		t.Fatalf("config: %+v", config)
	}

	// Every published port is on loopback.
	ps, err := exec.Command("docker", "ps", "--filter", "label=com.docker.compose.project="+project, "--format", "{{.Names}} {{.Ports}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps: %v\n%s", err, ps)
	}
	if strings.Count(string(ps), "\n") != 2 {
		t.Fatalf("want db and api running:\n%s", ps)
	}
	for _, field := range strings.FieldsFunc(string(ps), func(r rune) bool { return r == ' ' || r == ',' || r == '\n' }) {
		if strings.Contains(field, "->") && !strings.HasPrefix(field, "127.0.0.1:") {
			t.Fatalf("published beyond loopback: %s\n%s", field, ps)
		}
	}

	out, err = run(2*time.Minute, "backup")
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	dumps, _ := filepath.Glob(filepath.Join(dir, "backups", "*.dump"))
	if len(dumps) != 1 {
		t.Fatalf("dumps: %v\n%s", dumps, out)
	}
	sum, err := os.ReadFile(strings.TrimSuffix(dumps[0], ".dump") + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	dump, _ := os.Open(dumps[0])
	h := sha256.New()
	_, _ = io.Copy(h, dump)
	_ = dump.Close()
	if want := hex.EncodeToString(h.Sum(nil)) + "  " + filepath.Base(dumps[0]) + "\n"; string(sum) != want {
		t.Fatalf("checksum file %q, want %q", sum, want)
	}

	if out, err = run(time.Minute, "status"); err != nil || !strings.Contains(out, `"status":"ok"`) {
		t.Fatalf("status: %v\n%s", err, out)
	}

	out, err = run(2*time.Minute, "uninstall", "--delete-data", "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	uninstalled = true
	if exec.Command("docker", "volume", "inspect", project+"_postgres-data").Run() == nil {
		t.Fatal("the database volume survived uninstall --delete-data")
	}
	if exec.Command("docker", "image", "inspect", project+"-api").Run() == nil {
		t.Fatal("the built image survived uninstall")
	}
	if _, err = os.Stat(filepath.Join(dir, "clawbridge.env")); err != nil {
		t.Fatal("uninstall removed the env file; it keeps the folder")
	}
}

// cleanupBudget bounds TestLocalHostDocker's teardown, outside the test's own
// deadline.
const cleanupBudget = 3 * time.Minute

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	if err = json.Unmarshal(body, into); err != nil {
		t.Fatalf("GET %s: %v %s", url, err, body)
	}
}

// Every docker call gets the env file, never the shell: replayed against the
// real Compose, the shell's values lose to the file's.
func TestLocalHostEnvIsolation(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	dir := filepath.Join(t.TempDir(), "server")
	r := localHostFake(t, dir, "", nil, "up", "--owner", "owner@example.test", "--port", "18443")
	if r.code != 0 {
		t.Fatal(r.out)
	}
	envPath := filepath.Join(dir, "clawbridge.env")
	argvs, _ := filepath.Glob(filepath.Join(r.bin, "call-*.argv"))
	var up []string
	for _, path := range argvs {
		raw, _ := os.ReadFile(path)
		args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		for i := range args {
			if args[i] == "up" && i+1 < len(args) && args[i+1] == "-d" {
				up = args[:i]
				envRaw, _ := os.ReadFile(strings.TrimSuffix(path, ".argv") + ".env")
				var stray []string
				for _, line := range strings.Split(string(envRaw), "\n") {
					if strings.HasPrefix(line, "POSTGRES_") || strings.HasPrefix(line, "CLAWBRIDGE_") &&
						!strings.HasPrefix(line, "CLAWBRIDGE_LOCAL_UID=") && !strings.HasPrefix(line, "CLAWBRIDGE_LOCAL_GID=") {
						stray = append(stray, line)
					}
				}
				if len(stray) != 0 {
					t.Fatalf("compose up saw a ClawBridge variable from the shell:\n%s", envRaw)
				}
				if !strings.Contains(string(envRaw), "PATH=") {
					t.Fatalf("the fake did not record the environment: %q", envRaw)
				}
			}
		}
	}
	if len(up) < 3 || up[0] != "compose" || !strings.Contains(strings.Join(up, " "), "--env-file "+envPath) {
		t.Fatalf("no compose up call with the env file: %q", up)
	}
	// The same arguments, rendered for real.
	cmd := exec.Command("docker", append(up, "config", "--format", "json")...)
	// As clean_env passes it: no ClawBridge variable from the shell.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	var doc localRendered
	if err = json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	env := readEnvFile(t, envPath)
	db, api := doc.Services["db"], doc.Services["api"]
	if db.Environment["POSTGRES_PASSWORD"] != env["POSTGRES_PASSWORD"] || api.Environment["CLAWBRIDGE_BOOTSTRAP_OWNER"] != "owner@example.test" {
		t.Fatal("the env file's values did not reach compose")
	}
	if !strings.Contains(strings.Join(api.Command, " "), "--public-origin http://127.0.0.1:18443") || api.Environment["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"] != "" {
		t.Fatalf("api: %q %q", api.Command, api.Environment["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"])
	}
	// And had the shell's values got through, they would have won: this
	// is what clean_env prevents.
	leaky := exec.Command("docker", append(up, "config", "--format", "json")...)
	leaky.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, shellSecrets...)
	if out, err = leaky.CombinedOutput(); err != nil || !strings.Contains(string(out), "from-the-shell") {
		t.Fatalf("the shell's values would not override the env file, so this test proves nothing: %v", err)
	}
}

// On a re-run, a workspace the owner already owns counts as invited: no new
// recovery code, the marker written, and up succeeds. Another owner's
// workspace is reported, not fatal.
func TestLocalHostInviteRerun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	if out, code, _ := localHostRun(t, dir, "up", "--owner", "Owner@Example.test"); code != 0 {
		t.Fatal(out)
	}
	marker := filepath.Join(dir, "owner-invited")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a non-terminal up wrote the marker")
	}
	other := localHostFake(t, dir, "other@example.test", nil, "up")
	if other.code != 0 || !strings.Contains(other.out, "another owner") || strings.Contains(other.calls, "bootstrap-invite") {
		t.Fatalf("another owner: %d\n%s\n%s", other.code, other.out, other.calls)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker written for another owner's workspace")
	}
	mine := localHostFake(t, dir, "owner@example.test", nil, "up")
	if mine.code != 0 || !strings.Contains(mine.out, "already belongs") || strings.Contains(mine.calls, "bootstrap-invite") {
		t.Fatalf("own workspace: %d\n%s\n%s", mine.code, mine.out, mine.calls)
	}
	if !strings.Contains(mine.calls, "exec -T db psql -U clawbridge -d clawbridge -tAq -c SELECT owner_principal FROM workspaces WHERE name = 'my-team'") {
		t.Fatalf("psql call: %s", mine.calls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("marker not written for the owner's own workspace")
	}
}

// The folder: made 0700 when the script creates it. An existing one that
// group or others can enter or write is refused, with the fix, and left as
// it was; tls/ is forced to 0700 every time and the key copied 0600. Without
// hard links, the env file is still made once, 0600.
func TestLocalHostFolderAndNoHardLinks(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o710, 0o701, 0o720, 0o702} {
		existing := t.TempDir()
		if err := os.Chmod(existing, mode); err != nil {
			t.Fatal(err)
		}
		out, code, ran := localHostRun(t, existing, "up", "--owner", "owner@example.test")
		if code != 1 || !strings.Contains(out, "chmod 700 "+existing) {
			t.Fatalf("existing %v folder: %d\n%s", mode, code, out)
		}
		if info, _ := os.Stat(existing); info.Mode().Perm() != mode {
			t.Fatalf("an existing folder's mode changed to %v", info.Mode().Perm())
		}
		if _, err := os.Stat(filepath.Join(existing, "clawbridge.env")); err == nil || strings.Contains(ran, "compose --project-name") {
			t.Fatalf("%v: went ahead anyway: %s", mode, ran)
		}
	}

	// A 0700 folder with a 0755 tls/ from before: tls/ ends 0700, the key 0600.
	open := t.TempDir()
	if err := os.Chmod(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(open, "tls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(open, "tls"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for name, mode := range map[string]os.FileMode{"cert.pem": 0o644, "key.pem": 0o644} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("synthetic"), mode); err != nil {
			t.Fatal(err)
		}
	}
	out, code, _ := localHostRun(t, open, "up", "--owner", "owner@example.test", "--lan", "clawbridge.lan",
		"--cert", filepath.Join(src, "cert.pem"), "--key", filepath.Join(src, "key.pem"))
	if code != 0 {
		t.Fatalf("tls up: %d\n%s", code, out)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(open, "tls"): 0o700, filepath.Join(open, "tls", "key.pem"): 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", path, info, err, want)
		}
	}
	// A later command refuses a folder opened since.
	if err := os.Chmod(open, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, code, _ = localHostRun(t, open, "status"); code != 1 || !strings.Contains(out, "chmod 700") {
		t.Fatalf("status in an opened folder: %d\n%s", code, out)
	}

	dir := filepath.Join(t.TempDir(), "server")
	noLinks := map[string]string{"ln": "#!/bin/sh\necho 'ln: hard links not supported' >&2\nexit 1\n"}
	r := localHostFake(t, dir, "", noLinks, "up", "--owner", "owner@example.test")
	if r.code != 0 || !strings.Contains(r.out, "doesn't support hard links") {
		t.Fatalf("no hard links: %d\n%s", r.code, r.out)
	}
	info, err := os.Stat(filepath.Join(dir, "clawbridge.env"))
	if err != nil || info.Mode().Perm() != 0o600 || len(readEnvFile(t, filepath.Join(dir, "clawbridge.env"))["POSTGRES_PASSWORD"]) != 64 {
		t.Fatalf("env file without hard links: %v %v", info, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.partial*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// compose.local.yaml's !override needs Compose 2.24.4 or newer: an older one
// is refused with the reason, before any compose call.
func TestLocalHostComposeVersion(t *testing.T) {
	for version, ok := range map[string]bool{
		"2.24.4": true, "v2.24.4": true, "2.29.1-desktop.1": true, "2.30": true, "5.5.0": true,
		"2.24.3": false, "2.23.9": false, "1.29.2": false, "v2.20.2-desktop.1": false, "garbage": false,
	} {
		dir := filepath.Join(t.TempDir(), "server")
		r := localHostFake(t, dir, "", map[string]string{"compose.version": version + "\n"}, "up", "--owner", "owner@example.test")
		if ok && r.code != 0 {
			t.Errorf("%s refused: %s", version, r.out)
		}
		if !ok && (r.code != 1 || !strings.Contains(r.out, "needs Compose 2.24.4 or newer") || strings.Contains(r.calls, "--project-name")) {
			t.Errorf("%s: %d %s\n%s", version, r.code, r.out, r.calls)
		}
	}
}

// LH1: a LAN server named by its IP address. Clients of an IP send no server
// name (Go's crypto/tls never does), and Caddy without default_sni ends their
// handshake with "internal error". Opt-in (CLAWBRIDGE_TEST_DOCKER=1): each
// Caddy service runs as compose.local.yaml renders it for --lan 127.0.0.1,
// on a free loopback port, and a Go client with no server name connects and
// verifies the certificate.
func TestLocalHostCaddyWithoutSNI(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_DOCKER") != "1" {
		t.Skip("set CLAWBRIDGE_TEST_DOCKER=1 to run Caddy in Docker")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local-host.sh is a POSIX shell script")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker is not running")
	}
	// handshake dials with no server name until the TLS session verifies
	// against roots, or the deadline passes; Caddy needs a moment to start.
	handshake := func(port int, roots func() *x509.CertPool) {
		t.Helper()
		var err error
		for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			pool := roots()
			if pool == nil {
				err = errors.New("the server's root certificate is not readable yet")
				continue
			}
			var conn *tls.Conn
			conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
			if err == nil {
				if sni := conn.ConnectionState().ServerName; sni != "" {
					t.Fatalf("the client sent a server name %q; the test needs none", sni)
				}
				conn.Close()
				return
			}
		}
		t.Fatalf("no verified handshake without a server name: %v", err)
	}
	run := func(name string, service localService, port int, mounts ...string) {
		t.Helper()
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
		args := []string{"run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:443", port), "-e", "CLAWBRIDGE_DOMAIN=" + service.Environment["CLAWBRIDGE_DOMAIN"]}
		for _, m := range mounts {
			args = append(args, "-v", m)
		}
		args = append(args, "--entrypoint", service.Entrypoint[0], service.Image)
		args = append(args, service.Entrypoint[1:]...)
		// compose config keeps the $$ escape; Compose itself unescapes it
		// when it starts the container.
		for _, c := range service.Command {
			args = append(args, strings.ReplaceAll(c, "$$", "$"))
		}
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker run %s: %v\n%s", name, err, out)
		}
	}

	// B1: Caddy's internal CA; the root comes from the running container.
	lan := filepath.Join(t.TempDir(), "server")
	if out, code, _ := localHostRun(t, lan, "up", "--owner", "owner@example.test", "--lan", "127.0.0.1"); code != 0 {
		t.Fatal(out)
	}
	internal := fmt.Sprintf("cb-localhost-sni-%d", os.Getpid())
	port := freeLoopbackPort(t)
	run(internal, renderLocal(t, filepath.Join(lan, "clawbridge.env"), nil, "lan").Services["caddy"], port)
	handshake(port, func() *x509.CertPool {
		root, err := exec.Command("docker", "exec", internal, "cat", "/data/caddy/pki/authorities/local/root.crt").Output()
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(root) {
			return nil
		}
		return pool
	})

	// B2/B3: a brought certificate for the IP address.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	certFile, keyFile := filepath.Join(src, "cert.pem"), filepath.Join(src, "key.pem")
	if err = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	// Readable by the container's user: this test runs it without
	// local-host.sh's uid mapping.
	if err = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	brought := filepath.Join(t.TempDir(), "server")
	if out, code, _ := localHostRun(t, brought, "up", "--owner", "owner@example.test", "--lan", "127.0.0.1", "--cert", certFile, "--key", keyFile); code != 0 {
		t.Fatal(out)
	}
	cert := internal + "-cert"
	port = freeLoopbackPort(t)
	run(cert, renderLocal(t, filepath.Join(brought, "clawbridge.env"), nil, "lan-cert").Services["caddy-cert"], port,
		certFile+":/run/clawbridge-tls/cert.pem:ro", keyFile+":/run/clawbridge-tls/key.pem:ro")
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	handshake(port, func() *x509.CertPool { return pool })
}
