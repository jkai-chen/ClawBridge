package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/bus"
)

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

func healthServer(t *testing.T, status int, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A redirect leads somewhere healthy: the check must not follow it.
		if r.URL.Path == "/elsewhere" {
			_, _ = io.WriteString(w, `{"status":"ok","version":"1.2.3"}`)
			return
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		if r.URL.Path != "/healthz" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func cloudCheck(t *testing.T, args ...string) ([]cloudResult, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), append([]string{"cloud", "check", "--json"}, args...), &out, &bytes.Buffer{})
	var got struct {
		Source string        `json:"source"`
		Data   []cloudResult `json:"data"`
	}
	if e := json.Unmarshal(out.Bytes(), &got); e != nil || got.Source != "cloud_check" {
		t.Fatalf("output %q: %v", out.String(), e)
	}
	return got.Data, err
}

func TestCloudCheckServerAndVersion(t *testing.T) {
	isolated(t)
	for name, c := range map[string]struct {
		client, status, body string
		code                 int
		server, version      cloudResult
	}{
		"same release": {"1.2.3", "", `{"status":"ok","version":"1.2.3"}`, 200,
			cloudResult{"server", "pass", "", ""}, cloudResult{"version", "pass", "", ""}},
		"older server": {"1.2.4", "", `{"status":"ok","version":"1.2.3"}`, 200,
			cloudResult{"server", "pass", "", ""}, cloudResult{"version", "fail", "version_mismatch", "client 1.2.4, server 1.2.3: update the older one to the same release"}},
		"server predates the version": {"1.2.3", "", `{"status":"ok"}`, 200,
			cloudResult{"server", "pass", "", ""}, cloudResult{"version", "fail", "server_too_old", "the server predates version reporting; update it to 1.2.3"}},
		"dev client": {"dev", "", `{"status":"ok","version":"1.2.3"}`, 200,
			cloudResult{"server", "pass", "", ""}, cloudResult{"version", "warn", "dev_build", "a development build: versions not compared (client dev, server 1.2.3)"}},
		"dev server": {"1.2.3", "", `{"status":"ok","version":"dev"}`, 200,
			cloudResult{"server", "pass", "", ""}, cloudResult{"version", "warn", "dev_build", "a development build: versions not compared (client 1.2.3, server dev)"}},
		"not ok": {"1.2.3", "", `{"status":"starting","version":"1.2.3"}`, 200,
			failed("server", "not_clawbridge", "the URL answers but is not a ClawBridge server"), failed("version", "not_clawbridge", "the URL answers but is not a ClawBridge server")},
		"another site": {"1.2.3", "", `<html>`, 200,
			failed("server", "not_clawbridge", "the URL answers but is not a ClawBridge server"), failed("version", "not_clawbridge", "the URL answers but is not a ClawBridge server")},
		"unavailable": {"1.2.3", "", `{"status":"ok","version":"1.2.3"}`, 503,
			failed("server", "http_status", "GET /healthz answered 503; check the reverse proxy and that the server runs"), failed("version", "http_status", "GET /healthz answered 503; check the reverse proxy and that the server runs")},
		"redirected": {"1.2.3", "", `{"status":"ok","version":"1.2.3"}`, 302,
			failed("server", "http_status", "GET /healthz answered 302; check the reverse proxy and that the server runs"), failed("version", "http_status", "GET /healthz answered 302; check the reverse proxy and that the server runs")},
	} {
		withVersion(t, c.client)
		url := healthServer(t, c.code, c.body)
		got, err := cloudCheck(t, "server", "version", "--server", url)
		if !reflect.DeepEqual(got, []cloudResult{c.server, c.version}) {
			t.Fatalf("%s: %+v", name, got)
		}
		if wantFail := c.server.Status == "fail" || c.version.Status == "fail"; (err != nil) != wantFail {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

// Only the version check: it fetches /healthz on its own.
func TestCloudCheckVersionAlone(t *testing.T) {
	isolated(t)
	withVersion(t, "1.2.3")
	got, err := cloudCheck(t, "version", "--server", healthServer(t, 200, `{"status":"ok","version":"1.2.3"}`))
	if err != nil || !reflect.DeepEqual(got, []cloudResult{{"version", "pass", "", ""}}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestCloudCheckTransportFailures(t *testing.T) {
	isolated(t)
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	for server, reason := range map[string]string{
		"":                              "no_server",
		"http://example.test":           "invalid_url",
		"ftp://127.0.0.1":               "invalid_url",
		"https://user@example.test":     "invalid_url",
		"https://example.test/?x=1":     "invalid_url",
		tlsServer.URL:                   "tls",
		"https://name-does-not.invalid": "dns",
		"http://127.0.0.1:1":            "unreachable",
	} {
		args := []string{"server"}
		if server != "" {
			args = append(args, "--server", server)
		}
		got, err := cloudCheck(t, args...)
		if err == nil || len(got) != 1 || got[0].Status != "fail" || got[0].Reason != reason || got[0].Advice == "" {
			t.Fatalf("%q: %+v %v", server, got, err)
		}
	}
}

func TestCloudCheckLoopbackOnlyOverHTTP(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://example.test":  true,
		"http://localhost:8080": true,
		"http://127.0.0.1:8080": true,
		"http://[::1]:8080":     true,
		"http://10.0.0.1":       false,
		"http://example.test":   false,
		"https://":              false,
	} {
		if _, got := serverURL(raw); got != ok {
			t.Fatalf("%s: %v", raw, got)
		}
	}
}

func TestCloudCheckRejectsUnknownChecksAndPrintsATable(t *testing.T) {
	isolated(t)
	if err := Run(context.Background(), []string{"cloud", "check", "infra"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown check") {
		t.Fatal(err)
	}
	withVersion(t, "1.2.3")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"cloud", "check", "server", "--server", healthServer(t, 200, `{"status":"ok","version":"1.2.3"}`)}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "CHECK") || strings.Fields(lines[1])[1] != "pass" {
		t.Fatalf("table: %q", out.String())
	}
}

type fakeCloud struct {
	resolveErr, whoamiErr error
	data                  any
	asked                 []bus.Request
}

func (f *fakeCloud) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.asked = append(f.asked, r)
	return bus.Result{Data: f.data}, f.whoamiErr
}

func (f *fakeCloud) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "wsp_1", "prj_1", f.resolveErr
}

func TestCloudCheckWorkspace(t *testing.T) {
	cloud := Config{Mode: "cloud", Server: "https://bridge.example.test"}
	owner := map[string]any{"principal": "owner@example.test", "workspace": map[string]any{"owner_principal": "owner@example.test"}}
	member := map[string]any{"principal": "member@example.test", "workspace": map[string]any{"owner_principal": "owner@example.test"}}
	for name, c := range map[string]struct {
		probe     cloudProbe
		newErr    error
		fake      fakeCloud
		reason    string
		wantAsked bool
	}{
		"owner":            {cloudProbe{config: cloud}, nil, fakeCloud{data: owner}, "", true},
		"same server":      {cloudProbe{config: cloud, server: "https://bridge.example.test/"}, nil, fakeCloud{data: owner}, "", true},
		"member":           {cloudProbe{config: cloud}, nil, fakeCloud{data: member}, "not_owner", true},
		"no principal":     {cloudProbe{config: cloud}, nil, fakeCloud{data: map[string]any{"workspace": map[string]any{"owner_principal": ""}}}, "not_owner", true},
		"no workspace row": {cloudProbe{config: cloud}, nil, fakeCloud{data: map[string]any{"principal": "owner@example.test"}}, "not_owner", true},
		"no config":        {cloudProbe{configErr: errors.New("x")}, nil, fakeCloud{data: owner}, "no_config", false},
		"local":            {cloudProbe{config: Config{Mode: "local"}}, nil, fakeCloud{data: owner}, "not_cloud", false},
		"other server":     {cloudProbe{config: cloud, server: "https://other.example.test"}, nil, fakeCloud{data: owner}, "other_server", false},
		"no credentials":   {cloudProbe{config: cloud}, errors.New("x"), fakeCloud{data: owner}, "not_signed_in", false},
		"no project":       {cloudProbe{config: cloud}, nil, fakeCloud{data: owner, resolveErr: errors.New("x")}, "no_workspace", false},
		"whoami fails":     {cloudProbe{config: cloud}, nil, fakeCloud{data: owner, whoamiErr: errors.New("x")}, "whoami_failed", true},
	} {
		fake := c.fake
		old := newCloudExecutor
		newCloudExecutor = func(Config) (interface {
			Execute(context.Context, bus.Request) (bus.Result, error)
			ResolveProject(context.Context, string, string, string) (string, string, error)
		}, error) {
			return &fake, c.newErr
		}
		probe := c.probe
		got := checkWorkspace(context.Background(), &probe)
		newCloudExecutor = old
		want := cloudResult{Check: "workspace", Status: "pass"}
		if c.reason != "" {
			want = cloudResult{Check: "workspace", Status: "fail", Reason: c.reason, Advice: got.Advice}
		}
		if got != want || (c.reason != "" && got.Advice == "") {
			t.Fatalf("%s: %+v", name, got)
		}
		if asked := len(fake.asked) == 1 && fake.asked[0].Operation == "whoami" && fake.asked[0].Workspace == "wsp_1" && fake.asked[0].Project == "prj_1"; asked != c.wantAsked {
			t.Fatalf("%s: asked %+v", name, fake.asked)
		}
	}
}

type remoteCall struct {
	argv  []string
	stdin string
}

func stubSecretIO(t *testing.T, stdin string, terminal bool, remoteErr error) *[]remoteCall {
	t.Helper()
	calls := &[]remoteCall{}
	oldIn, oldTerm, oldHidden, oldRemote := cloudStdin, stdinTerminal, readHidden, runRemote
	cloudStdin = strings.NewReader(stdin)
	stdinTerminal = func() bool { return terminal }
	readHidden = func() ([]byte, error) { return []byte(stdin), nil }
	runRemote = func(_ context.Context, argv []string, in io.Reader, _ io.Writer) error {
		b, _ := io.ReadAll(in)
		*calls = append(*calls, remoteCall{argv, string(b)})
		return remoteErr
	}
	t.Cleanup(func() { cloudStdin, stdinTerminal, readHidden, runRemote = oldIn, oldTerm, oldHidden, oldRemote })
	return calls
}

func secretSet(args ...string) (string, error) {
	var out bytes.Buffer
	err := Run(context.Background(), append([]string{"cloud", "secret", "set"}, args...), &out, &bytes.Buffer{})
	return out.String(), err
}

func TestCloudSecretSetSendsTheValueOnStdinOnly(t *testing.T) {
	const value = "lin_synthetic-secret_0123"
	key := "CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET"
	script := `sudo -n sh -c '` + remoteSetScript(key, serverEnvFile) + `'`
	for name, c := range map[string]struct {
		args     []string
		terminal bool
		argv     []string
	}{
		"ssh, hidden prompt": {[]string{key, "--ssh", "admin@bridge.example.test"}, true,
			[]string{"ssh", "-T", "admin@bridge.example.test", script}},
		"gcloud, piped": {[]string{key, "--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--value-stdin"}, false,
			[]string{"gcloud", "compute", "ssh", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--tunnel-through-iap", "--command", script}},
	} {
		calls := stubSecretIO(t, value+"\n", c.terminal, nil)
		out, err := secretSet(c.args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].argv, c.argv) || (*calls)[0].stdin != value+"\n" {
			t.Fatalf("%s: %+v", name, *calls)
		}
		if strings.Contains(strings.Join((*calls)[0].argv, " "), value) || strings.Contains(out, value) || !strings.Contains(out, `"set":true`) {
			t.Fatalf("%s: value leaked or not set: %q", name, out)
		}
	}
}

func TestCloudSecretSetRefusesBadInput(t *testing.T) {
	key := "CLAWBRIDGE_LINEAR_APP_CLIENT_ID"
	for name, c := range map[string]struct {
		args     []string
		stdin    string
		terminal bool
		want     string
	}{
		"no key":             {[]string{"--ssh", "host"}, "v", true, "one KEY"},
		"unknown key":        {[]string{"DATABASE_URL", "--ssh", "host"}, "v", true, "one KEY"},
		"two keys":           {[]string{key, key, "--ssh", "host"}, "v", true, "one KEY"},
		"no transport":       {[]string{key}, "v", true, "needs --ssh"},
		"both transports":    {[]string{key, "--ssh", "host", "--vm", "vm-1", "--project", "example-project", "--zone", "europe-west1-b"}, "v", true, "needs --ssh"},
		"ssh option host":    {[]string{key, "--ssh", "-oProxyCommand=x"}, "v", true, "--ssh must"},
		"ssh spaced host":    {[]string{key, "--ssh", "host x"}, "v", true, "--ssh must"},
		"bad vm":             {[]string{key, "--vm", "VM;x", "--project", "example-project", "--zone", "europe-west1-b"}, "v", true, "valid Compute Engine"},
		"bad project":        {[]string{key, "--vm", "vm-1", "--project", "x", "--zone", "europe-west1-b"}, "v", true, "valid Compute Engine"},
		"bad zone":           {[]string{key, "--vm", "vm-1", "--project", "example-project", "--zone", "europe west1"}, "v", true, "valid Compute Engine"},
		"pipe, no flag":      {[]string{key, "--ssh", "host"}, "v", false, "not a terminal"},
		"empty":              {[]string{key, "--ssh", "host"}, "\n", true, "empty value"},
		"two lines":          {[]string{key, "--ssh", "host", "--value-stdin"}, "a\nb\n", false, "one line"},
		"space":              {[]string{key, "--ssh", "host"}, "a b", true, "unsupported character"},
		"quote":              {[]string{key, "--ssh", "host"}, "a'b", true, "unsupported character"},
		"dollar":             {[]string{key, "--ssh", "host"}, "a$b", true, "unsupported character"},
		"backtick":           {[]string{key, "--ssh", "host"}, "a`b", true, "unsupported character"},
		"non-ascii":          {[]string{key, "--ssh", "host"}, "aé", true, "unsupported character"},
		"too long, prompt":   {[]string{key, "--ssh", "host"}, strings.Repeat("a", 4097), true, "longer than 4096"},
		"too long, piped":    {[]string{key, "--ssh", "host", "--value-stdin"}, strings.Repeat("a", 5000), false, "longer than 4096"},
		"unknown subcommand": {[]string{}, "", true, ""},
	} {
		calls := stubSecretIO(t, c.stdin, c.terminal, nil)
		var err error
		if name == "unknown subcommand" {
			err = Run(context.Background(), []string{"cloud", "secret", "get", key}, &bytes.Buffer{}, &bytes.Buffer{})
			c.want = "unknown cloud command"
		} else {
			_, err = secretSet(c.args...)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || len(*calls) != 0 {
			t.Fatalf("%s: %v, %d remote calls", name, err, len(*calls))
		}
	}
	// The longest accepted value, and a remote failure that never echoes it.
	value := strings.Repeat("a", 4096)
	calls := stubSecretIO(t, value, true, errors.New("exit status 255: "+value))
	if _, err := secretSet(key, "--ssh", "host"); err == nil || strings.Contains(err.Error(), value) || len(*calls) != 1 {
		t.Fatalf("remote failure: %v", err)
	}
}

// The remote script, run here against a temporary file: it replaces only its
// key's line, keeps the rest, and installs the file 0600.
func TestRemoteSetScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on the Linux server; Windows has no POSIX sh for its paths")
	}
	path := filepath.Join(t.TempDir(), "conf", "clawbridge.env")
	key := "CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET"
	script := remoteSetScript(key, path)
	if strings.Contains(script, "'") {
		t.Fatal("the script has a single quote")
	}
	local := strings.Replace(script, `chown 0:0 "$t"`, ":", 1)
	if local == script {
		t.Fatal("no chown to strip")
	}
	run := func(stdin string) error {
		cmd := exec.Command("sh", "-c", local)
		cmd.Stdin = strings.NewReader(stdin)
		return cmd.Run()
	}
	if err := run("first\n"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("created directory: %v %v", info, err)
	}
	// A file holding only this key: nothing else survives the filter.
	if err := run("second"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != key+"=second\n" {
		t.Fatalf("env file: %q", got)
	}
	if err := os.WriteFile(path, []byte("A=1\n"+key+"=old\n"+key+"_X=keep\nB=2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run("new-value"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "A=1\n"+key+"_X=keep\nB=2\n"+key+"=new-value\n" {
		t.Fatalf("env file: %q", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("env file: %v %v", info, err)
	}
	for _, empty := range []string{"", "\n"} {
		if err := run(empty); err == nil {
			t.Fatalf("an empty value %q was written", empty)
		}
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, got) {
		t.Fatal("a failed write changed the file")
	}
	if left, _ := filepath.Glob(path + ".*"); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// A linked env file keeps its link; the target gets the line.
	target := filepath.Join(t.TempDir(), "real.env")
	if err := os.WriteFile(target, []byte("A=1\n"+key+"=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := run("linked"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "A=1\n"+key+"=linked\n" {
		t.Fatalf("target: %q", got)
	}
	if left, _ := filepath.Glob(target + ".*"); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	// A link to something other than a regular file is refused.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if err := run("refused"); err == nil {
		t.Fatal("wrote through a link to a directory")
	}
}
