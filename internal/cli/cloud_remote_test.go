package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// The required settings are exactly the compose file's `:?` entries.
func TestRequiredSettingsMatchCompose(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\{([A-Z_]+):\?`).FindAllStringSubmatch(string(raw), -1) {
		seen[m[1]] = true
	}
	var compose []string
	for k := range seen {
		compose = append(compose, k)
	}
	sort.Strings(compose)
	if !reflect.DeepEqual(compose, requiredSettings) {
		t.Fatalf("compose requires %v, the check counts %v", compose, requiredSettings)
	}
}

// The facts script, run here: counts only non-empty lines, prints no value,
// and lists only this deployment's containers.
func TestFactsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on the Linux server")
	}
	script := factsScriptFor("/nonexistent")
	if strings.Contains(script, "'") {
		t.Fatal("the script has a single quote")
	}
	dir := t.TempDir()
	env := filepath.Join(dir, "clawbridge.env")
	// The last CLAWBRIDGE_AUTH counts, and only its letters and commas
	// are printed.
	body := "POSTGRES_PASSWORD=value-sentinel\nCLAWBRIDGE_DOMAIN=\nCLAWBRIDGE_AUDIENCE=a\nCLAWBRIDGE_AUDIENCE=b\n# CLAWBRIDGE_BOOTSTRAP_OWNER=x\nCOMPOSE_PROJECT_NAME=my-stack\nCLAWBRIDGE_AUTH=google\nCLAWBRIDGE_AUTH=Device;sentinel=1\n"
	if err := os.WriteFile(env, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	// A docker that answers only for this project's label.
	// A docker that answers only for this project's label, newest first, and
	// lists `compose run` one-offs unless they are filtered out: an exited
	// one-off api, and a running one-off api older than a crashed service
	// api.
	docker := `#!/bin/sh
case "$*" in *label=com.docker.compose.project=ok-stack*)
  echo "service caddy running Up 3 hours"; echo "service api running Up 1 minute"
  case "$*" in *label=com.docker.compose.oneoff=False*) ;; *) echo "service api exited Exited (1) 2 hours ago";; esac
  echo "service db running Up 3 hours (healthy)"; exit 0;;
esac
case "$*" in *label=com.docker.compose.project=my-stack*) ;; *) exit 0;; esac
echo "service api exited Exited (1) 1 minute ago"
case "$*" in *label=com.docker.compose.oneoff=False*) ;; *) echo "service api exited Exited (0) 2 hours ago"; echo "service api running Up 3 hours";; esac
echo "service db running Up 2 hours (healthy)"
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0700); err != nil {
		t.Fatal(err)
	}
	findmnt := "#!/bin/sh\necho /dev/sdb\n"
	if err := os.WriteFile(filepath.Join(dir, "findmnt"), []byte(findmnt), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(path string) remoteFacts {
		cmd := exec.Command("sh", "-c", factsScriptFor(path))
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "sentinel") {
			t.Fatal("a value was printed")
		}
		return parseFacts(out)
	}
	f := run(env)
	want := map[string]int{"POSTGRES_PASSWORD": 1, "CLAWBRIDGE_DOMAIN": 0, "CLAWBRIDGE_AUDIENCE": 2, "CLAWBRIDGE_BOOTSTRAP_OWNER": 0, "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY": 0, "CLAWBRIDGE_AUTH": 2}
	for k, n := range want {
		if got, ok := f.counts[k]; !ok || got != n {
			t.Fatalf("%s: %d (%v), want %d", k, got, ok, n)
		}
	}
	if len(f.counts) != len(requiredSettings)+len(googleSettings)+len(appSettings)+1 || f.envMissing || f.auth != "invalid" {
		t.Fatalf("facts: %+v", f)
	}
	// Only the service containers count, and the crashed api fails the check
	// although an older one-off api still runs.
	if !reflect.DeepEqual(f.services, map[string][]string{"db": {"running Up 2 hours (healthy)"}, "api": {"exited Exited (1) 1 minute ago"}}) {
		t.Fatalf("services: %v", f.services)
	}
	if r := checkServices(f); r.Reason != "service_down" || !strings.Contains(r.Advice, "api, caddy") {
		t.Fatalf("%+v", r)
	}
	// A stale, exited one-off beside a healthy service api does not fail it.
	ok := filepath.Join(dir, "ok.env")
	if err := os.WriteFile(ok, []byte("COMPOSE_PROJECT_NAME=ok-stack\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := checkServices(run(ok)); r.Status != "pass" {
		t.Fatalf("%+v", r)
	}
	device := filepath.Join(dir, "device.env")
	if err := os.WriteFile(device, []byte("CLAWBRIDGE_AUTH=\"device, google\" \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := run(device).auth; got != "device,google" {
		t.Fatalf("auth %q", got)
	}
	if f.mount != "/dev/sdb" {
		t.Fatalf("mount %q", f.mount)
	}
	if missing := run(filepath.Join(dir, "absent.env")); !missing.envMissing || len(missing.counts) != 0 || missing.mount != "/dev/sdb" {
		t.Fatalf("missing env: %+v", missing)
	}
}

func TestParseFactsIgnoresOtherLines(t *testing.T) {
	f := parseFacts([]byte("Warning: Permanently added host\ncount A 1\ncount B x\ncount C 1 2\nservice api running Up\nservice\nenv missing now\nmount /dev/sdb extra\n"))
	if !reflect.DeepEqual(f.counts, map[string]int{"A": 1}) || !reflect.DeepEqual(f.services, map[string][]string{"api": {"running Up"}}) || f.envMissing || f.mount != "" {
		t.Fatalf("%+v", f)
	}
}

func allSet() remoteFacts {
	f := remoteFacts{counts: map[string]int{}, services: map[string][]string{
		"db": {"running Up 1 hour (healthy)"}, "api": {"running Up 1 hour"}, "caddy": {"running Up 1 hour"},
	}}
	for _, k := range append(append(append([]string{}, requiredSettings...), googleSettings...), appSettings...) {
		f.counts[k] = 1
	}
	return f
}

func TestSettingsAndServices(t *testing.T) {
	for name, c := range map[string]struct {
		edit   func(*remoteFacts)
		reason string
	}{
		"healthy":        {func(*remoteFacts) {}, ""},
		"no env file":    {func(f *remoteFacts) { f.envMissing = true }, "env_missing"},
		"missing":        {func(f *remoteFacts) { f.counts["CLAWBRIDGE_DOMAIN"] = 0 }, "setting_missing"},
		"never counted":  {func(f *remoteFacts) { delete(f.counts, "POSTGRES_PASSWORD") }, "setting_missing"},
		"twice":          {func(f *remoteFacts) { f.counts["CLAWBRIDGE_AUDIENCE"] = 2 }, "setting_duplicate"},
		"google partial": {func(f *remoteFacts) { f.counts["CLAWBRIDGE_OAUTH_CLIENT_SECRET"] = 0 }, "setting_missing"},
		"device only": {func(f *remoteFacts) {
			f.auth = "device"
			for _, k := range googleSettings {
				f.counts[k] = 0
			}
		}, ""},
		// R67-L1: no Google settings, but Google sign-in still on.
		"no google, auth unset": {func(f *remoteFacts) {
			for _, k := range googleSettings {
				f.counts[k] = 0
			}
		}, "setting_missing"},
		"no google, auth both": {func(f *remoteFacts) {
			f.auth = "google,device"
			for _, k := range googleSettings {
				f.counts[k] = 0
			}
		}, "setting_missing"},
		"device with google set": {func(f *remoteFacts) { f.auth = "device" }, ""},
		"auth invalid":           {func(f *remoteFacts) { f.auth = "invalid" }, "setting_invalid"},
		"auth twice":             {func(f *remoteFacts) { f.counts["CLAWBRIDGE_AUTH"] = 2 }, "setting_duplicate"},
		"api absent":             {func(f *remoteFacts) { delete(f.services, "api") }, "service_down"},
		"caddy exited":           {func(f *remoteFacts) { f.services["caddy"] = []string{"exited Exited (1) 2 minutes ago"} }, "service_down"},
		"db no health":           {func(f *remoteFacts) { f.services["db"] = []string{"running Up 1 hour"} }, "service_down"},
		"db starting":            {func(f *remoteFacts) { f.services["db"] = []string{"running Up 3 seconds (health: starting)"} }, "service_down"},
		"api starting":           {func(f *remoteFacts) { f.services["api"] = []string{"running Up 2 seconds (health: starting)"} }, "service_down"},
		"api unhealthy":          {func(f *remoteFacts) { f.services["api"] = []string{"running Up 1 hour (unhealthy)"} }, "service_down"},
		"a crashed replica": {func(f *remoteFacts) {
			f.services["api"] = append([]string{"exited Exited (2) 1 minute ago"}, f.services["api"]...)
		}, "service_down"},
		"two healthy":    {func(f *remoteFacts) { f.services["caddy"] = append(f.services["caddy"], "running Up 2 hours") }, ""},
		"restarting api": {func(f *remoteFacts) { f.services["api"] = []string{"restarting Restarting (1) 3 seconds ago"} }, "service_down"},
	} {
		f := allSet()
		c.edit(&f)
		r := checkSettings(f)
		if r.Status == "pass" {
			r = checkServices(f)
		}
		if r.Reason != c.reason || (c.reason == "") != (r.Status == "pass") || (c.reason != "" && r.Advice == "") {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// The advice names the keys and services, never a value.
	f := allSet()
	f.counts["CLAWBRIDGE_DOMAIN"], f.counts["POSTGRES_PASSWORD"] = 0, 0
	if r := checkSettings(f); r.Advice != "set in the env file: CLAWBRIDGE_DOMAIN, POSTGRES_PASSWORD" {
		t.Fatal(r.Advice)
	}
}

type remoteRun struct{ argv []string }

func stubRemote(t *testing.T, out string, err error) *[]remoteRun {
	t.Helper()
	runs := &[]remoteRun{}
	old := remoteOutput
	remoteOutput = func(_ context.Context, argv []string, _ io.Writer) ([]byte, error) {
		*runs = append(*runs, remoteRun{argv})
		return []byte(out), err
	}
	t.Cleanup(func() { remoteOutput = old })
	return runs
}

func factsOutput(f remoteFacts) string {
	var b strings.Builder
	b.WriteString("auth " + f.auth + "\n")
	for k, n := range f.counts {
		b.WriteString("count " + k + " " + string(rune('0'+n)) + "\n")
	}
	for s, states := range f.services {
		for _, state := range states {
			b.WriteString("service " + s + " " + state + "\n")
		}
	}
	return b.String()
}

func TestCloudCheckServerOverSSH(t *testing.T) {
	isolated(t)
	withVersion(t, "1.2.3")
	url := healthServer(t, 200, `{"status":"ok","version":"1.2.3"}`)
	runs := stubRemote(t, factsOutput(allSet()), nil)
	got, err := cloudCheck(t, "server", "--server", url, "--ssh", "admin@bridge.example.test")
	if err != nil || !reflect.DeepEqual(got, []cloudResult{{"server", "pass", "", ""}}) {
		t.Fatalf("%+v %v", got, err)
	}
	want := wantSSH("admin@bridge.example.test", `env LC_ALL=C sudo -n sh -c '`+factsScript()+`'`)
	if len(*runs) != 1 || !reflect.DeepEqual((*runs)[0].argv, want) {
		t.Fatalf("runs: %+v", *runs)
	}
	// Over IAP.
	runs = stubRemote(t, factsOutput(allSet()), nil)
	if _, err = cloudCheck(t, "server", "--server", url, "--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b"); err != nil || len(*runs) != 1 || (*runs)[0].argv[0] != "gcloud" || (*runs)[0].argv[len((*runs)[0].argv)-2] != "--command" {
		t.Fatalf("%+v %v", *runs, err)
	}
	// A missing setting fails the server check before its services.
	missing := allSet()
	missing.counts["CLAWBRIDGE_DOMAIN"] = 0
	stubRemote(t, factsOutput(missing), nil)
	if got, _ = cloudCheck(t, "server", "--server", url, "--ssh", "host"); got[0].Reason != "setting_missing" {
		t.Fatalf("%+v", got)
	}
	// A failed SSH is its own reason; a failed HTTPS check runs no SSH.
	stubRemote(t, "", errors.New("exit status 255"))
	if got, _ = cloudCheck(t, "server", "--server", url, "--ssh", "host"); got[0].Reason != "ssh_failed" {
		t.Fatalf("%+v", got)
	}
	runs = stubRemote(t, factsOutput(allSet()), nil)
	if got, _ = cloudCheck(t, "server", "--server", healthServer(t, 503, ""), "--ssh", "host"); got[0].Reason != "http_status" || len(*runs) != 0 {
		t.Fatalf("%+v %+v", got, *runs)
	}
	// A bad target is refused before anything runs.
	runs = stubRemote(t, "", nil)
	if err = Run(context.Background(), []string{"cloud", "check", "--ssh", "-oProxyCommand=x"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--ssh must") || len(*runs) != 0 {
		t.Fatalf("bad target: %v", err)
	}
}

func TestCloudCheckLinearApp(t *testing.T) {
	cloud := Config{Mode: "cloud", Server: "https://bridge.example.test"}
	configured := map[string]any{"configured": true, "app_user_id": "app-user", "teams_visible": map[string]any{"ENG": true, "OPS": true}}
	for name, c := range map[string]struct {
		target    cloudTarget
		facts     func(*remoteFacts)
		sshErr    error
		data      any
		err       error
		reason    string
		askedAPI  bool
		uncounted bool
	}{
		"configured":     {cloudTarget{host: "host"}, nil, nil, configured, nil, "", true, false},
		"no target":      {cloudTarget{}, nil, nil, configured, nil, "", true, true},
		"an entry unset": {cloudTarget{host: "host"}, func(f *remoteFacts) { f.counts["CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"] = 0 }, nil, configured, nil, "app_settings_missing", false, false},
		"an entry twice": {cloudTarget{host: "host"}, func(f *remoteFacts) { f.counts["CLAWBRIDGE_LINEAR_APP_CLIENT_ID"] = 2 }, nil, configured, nil, "app_settings_missing", false, false},
		"ssh fails":      {cloudTarget{host: "host"}, nil, errors.New("x"), configured, nil, "ssh_failed", false, false},
		"no app":         {cloudTarget{}, nil, nil, nil, &remote.HTTPError{Status: 400, Code: "invalid"}, "not_configured", true, false},
		"status fails":   {cloudTarget{}, nil, nil, nil, &remote.HTTPError{Status: 503, Code: "unavailable"}, "status_unreadable", true, false},
		"not configured": {cloudTarget{}, nil, nil, map[string]any{"configured": false}, nil, "status_unreadable", true, false},
		"binding":        {cloudTarget{}, nil, nil, map[string]any{"configured": true, "binding": "unpinned"}, nil, "binding_unreadable", true, false},
		"a hidden team":  {cloudTarget{}, nil, nil, map[string]any{"configured": true, "teams_visible": map[string]any{"ENG": true, "OPS": false}}, nil, "teams_hidden", true, false},
	} {
		f := allSet()
		if c.facts != nil {
			c.facts(&f)
		}
		stubRemote(t, factsOutput(f), c.sshErr)
		fake := &fakeCloud{data: c.data, whoamiErr: c.err}
		old := newCloudExecutor
		newCloudExecutor = func(Config) (cloudExecutor, error) { return fake, nil }
		got := checkLinearApp(context.Background(), &cloudProbe{config: cloud, target: c.target})
		newCloudExecutor = old
		if got.Check != "linear-app" || got.Reason != c.reason || (c.reason == "") != (got.Status == "pass") {
			t.Fatalf("%s: %+v", name, got)
		}
		if asked := len(fake.asked) == 1 && fake.asked[0].Operation == "linear_app_status"; asked != c.askedAPI {
			t.Fatalf("%s: asked %+v", name, fake.asked)
		}
		if c.uncounted != (got.Advice != "" && got.Status == "pass") {
			t.Fatalf("%s: advice %q", name, got.Advice)
		}
	}
	// The hidden teams are named in the advice.
	stubRemote(t, "", nil)
	fake := &fakeCloud{data: map[string]any{"configured": true, "teams_visible": map[string]any{"OPS": false, "ENG": false, "WEB": true}}}
	old := newCloudExecutor
	newCloudExecutor = func(Config) (cloudExecutor, error) { return fake, nil }
	defer func() { newCloudExecutor = old }()
	if got := checkLinearApp(context.Background(), &cloudProbe{config: cloud}); !strings.HasSuffix(got.Advice, "2 bound team(s): ENG, OPS") {
		t.Fatal(got.Advice)
	}
}

// server and linear-app share one remote run.
func TestCloudCheckRunsTheScriptOnce(t *testing.T) {
	isolated(t)
	withVersion(t, "1.2.3")
	runs := stubRemote(t, factsOutput(allSet()), nil)
	_, _ = cloudCheck(t, "server", "linear-app", "--server", healthServer(t, 200, `{"status":"ok","version":"1.2.3"}`), "--ssh", "host")
	if len(*runs) != 1 {
		t.Fatalf("%d remote runs", len(*runs))
	}
	_ = bus.Request{}
}

// The server check compares the env file's sign-in methods with the running
// server's /v1/config: an env file changed without recreating the api does
// not take effect.
func TestCloudCheckAuthMode(t *testing.T) {
	isolated(t)
	withVersion(t, "1.2.3")
	health := `{"status":"ok","version":"1.2.3"}`
	device := allSet()
	device.auth = "device"
	both := allSet()
	both.auth = "google,device"
	for name, c := range map[string]struct {
		facts  remoteFacts
		config string
		reason string
	}{
		"unset is google":           {allSet(), `{"auth_methods":["google"]}`, ""},
		"env device, server google": {device, `{"auth_methods":["google"]}`, "auth_mismatch"},
		"env google, server both":   {allSet(), `{"auth_methods":["google","device"]}`, "auth_mismatch"},
		"order does not matter":     {both, `{"auth_methods":["device","google"]}`, ""},
		"google-only server":        {allSet(), `{"browser_client_id":"x"}`, ""},
		"env device, google-only":   {device, `{"browser_client_id":"x"}`, "auth_mismatch"},
		"config unavailable":        {allSet(), "", "config_unreadable"},
	} {
		stubRemote(t, factsOutput(c.facts), nil)
		got, _ := cloudCheck(t, "server", "--server", configServer(t, 200, health, c.config), "--ssh", "admin@bridge.example.test")
		if len(got) != 1 || got[0].Reason != c.reason || (c.reason == "") != (got[0].Status == "pass") {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
