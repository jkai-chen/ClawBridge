package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/remote"
)

// bootstrapAWS is an in-process aws CLI for cloud bootstrap: it keeps every
// call and answers the script's run with answer.
type bootstrapAWS struct {
	mu     sync.Mutex
	calls  [][]string
	stdins []string
	answer string
	status string
	// keep fails the parameter's delete, and says it is still there.
	keep bool
}

func (a *bootstrapAWS) run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, argv)
	a.stdins = append(a.stdins, string(stdin))
	switch argv[1] + " " + argv[2] {
	case "sts get-caller-identity":
		return []byte(`{"Account":"123456789012"}`), nil
	case "ssm put-parameter":
		return []byte(`{"Version":1}`), nil
	case "ssm send-command":
		return []byte(`{"Command":{"CommandId":"cmd-1"}}`), nil
	case "ssm get-command-invocation":
		status := a.status
		if status == "" {
			status = "Success"
		}
		out, _ := json.Marshal(map[string]any{"Status": status, "StandardOutputContent": a.answer, "ResponseCode": 6})
		return out, nil
	case "ssm delete-parameter":
		if a.keep {
			return nil, errors.New("denied")
		}
		return []byte(`{}`), nil
	case "ssm describe-parameters":
		return []byte(`{"Parameters":[{"Name":"left"}]}`), nil
	}
	return nil, errors.New("unexpected aws call")
}

func (a *bootstrapAWS) ops() []string {
	var ops []string
	for _, c := range a.calls {
		ops = append(ops, c[1]+" "+c[2])
	}
	return ops
}

func stubBootstrap(t *testing.T, answer string) (*bootstrapAWS, *[]JoinParams) {
	t.Helper()
	a := &bootstrapAWS{answer: answer}
	oldAWS, oldJoin, oldProbe, oldGOOS := awsCLI, joinDevice, probeJoin, awsGOOS
	t.Cleanup(func() { awsCLI, joinDevice, probeJoin, awsGOOS = oldAWS, oldJoin, oldProbe, oldGOOS })
	awsCLI, awsGOOS = a.run, "linux"
	var joins []JoinParams
	joinDevice = func(_ context.Context, p JoinParams) (JoinResult, error) {
		joins = append(joins, p)
		return JoinResult{DeviceID: "dev_1", Principal: p.Email, Workspace: "acme", KeyStore: p.KeyStore, ConfigPath: "/x/config.toml"}, nil
	}
	return a, &joins
}

func bootstrap(args ...string) (string, error) {
	var out, errs bytes.Buffer
	err := Run(context.Background(), append([]string{"cloud", "bootstrap"}, args...), &out, &errs)
	return out.String() + errs.String(), err
}

var bootstrapArgs = []string{"--instance", ec2ID, "--region", "us-east-1", "--account", "123456789012", "--server", "https://cb.example.test", "--workspace", "acme"}

// The code this process made reaches the server only on put-parameter's
// stdin, never in send-command's request, any argv or the output; the
// owner joins with it in memory as the email the server names; the
// parameter is deleted last.
func TestCloudBootstrapOverSSM(t *testing.T) {
	a, joins := stubBootstrap(t, `invite {"invite_id":"inv_1","email":"owner@example.test","workspace":"acme","workspace_created":true,"expires_at":"2026-10-28T05:00:00Z","purpose":"recovery"}`+"\n")
	out, err := bootstrap(bootstrapArgs...)
	if err != nil {
		t.Fatal(err, out)
	}
	if len(*joins) != 1 {
		t.Fatal("joins:", *joins)
	}
	j := (*joins)[0]
	if _, ok := devicekey.NormalCode(j.Code); !ok || j.Email != "owner@example.test" || j.Server != "https://cb.example.test" || j.KeyStore != "keyring" {
		t.Fatalf("join: %+v", j)
	}
	want := []string{"sts get-caller-identity", "ssm put-parameter", "ssm send-command", "ssm get-command-invocation", "ssm delete-parameter"}
	if strings.Join(a.ops(), ",") != strings.Join(want, ",") {
		t.Fatal("calls:", a.ops())
	}
	for i, argv := range a.calls {
		if strings.Contains(strings.Join(argv, " "), j.Code) || strings.Contains(a.stdins[i], j.Code) != (a.ops()[i] == "ssm put-parameter") {
			t.Fatalf("%s carries the code", a.ops()[i])
		}
	}
	if !strings.Contains(a.stdins[2], "--workspace acme --code-stdin") || strings.Contains(a.stdins[2], "expect-owner") || !strings.Contains(a.stdins[2], "get-parameter --with-decryption --name /clawbridge/"+ec2ID+"/bootstrap-code") {
		t.Fatal("send-command:", a.stdins[2])
	}
	// --email goes to the server's --expect-owner, and is checked again
	// against the invite before any join.
	if _, err = bootstrap(append(bootstrapArgs, "--email", "someone@example.test")...); err == nil || !strings.Contains(err.Error(), "bootstrap owner is owner@example.test") || len(*joins) != 1 {
		t.Fatal(err, *joins)
	}
	if _, err = bootstrap(append(bootstrapArgs, "--email", "Owner@Example.test")...); err != nil || len(*joins) != 2 {
		t.Fatal(err)
	}
	if strings.Contains(out, j.Code) || !strings.Contains(out, `"source":"cloud_bootstrap"`) || !strings.Contains(out, `"workspace_created":true`) || !strings.Contains(out, `"principal":"owner@example.test"`) {
		t.Fatal("output:", out)
	}
}

// R128-M1: the invite was made but the parameter's delete failed: the
// owner still joins (which uses the code up), and is told where the
// parameter is.
func TestCloudBootstrapJoinsDespiteALeftParameter(t *testing.T) {
	a, joins := stubBootstrap(t, `invite {"invite_id":"inv_1","email":"owner@example.test","workspace":"acme","workspace_created":true,"expires_at":"x"}`+"\n")
	a.keep = true
	out, err := bootstrap(bootstrapArgs...)
	if err != nil || len(*joins) != 1 || !strings.Contains(out, `"source":"cloud_bootstrap"`) || !strings.Contains(out, "warning: parameter left behind: /clawbridge/"+ec2ID+"/bootstrap-code") || strings.Contains(out, (*joins)[0].Code) {
		t.Fatal(err, out)
	}
	// A failed script with a left parameter is still a failure, and says so.
	a, joins = stubBootstrap(t, "")
	a.keep, a.status = true, "Failed"
	if _, err = bootstrap(bootstrapArgs...); err == nil || !strings.Contains(err.Error(), "parameter left behind") || len(*joins) != 0 {
		t.Fatal(err)
	}
}

// A refusal comes back as a fixed reason, joins nothing, and still deletes
// the parameter; an answer that carries a code, or none, is refused.
func TestCloudBootstrapRefusals(t *testing.T) {
	for answer, want := range map[string]string{
		"refused clawbridge: CLAWBRIDGE_BOOTSTRAP_OWNER names another email than --expect-owner; nothing was created\n": "(owner_mismatch)",
		"refused clawbridge: the owner has 3 pending invites; revoke one first\n":                                       "(pending_invites)",
		"refused clawbridge: that workspace's owner is not the bootstrap owner\n":                                       "(not_owner)",
		"refused clawbridge: CLAWBRIDGE_BOOTSTRAP_OWNER is empty; it names the bootstrap owner\n":                       "(no_owner)",
		"refused clawbridge: flag provided but not defined: -code-stdin\n":                                              "(old_server)",
		"refused clawbridge: something else\n":                                                                          "(refused)",
		`invite {"email":"owner@example.test","code":"CBJ-AAAA-BBBB-CCCC-DDDD"}` + "\n":                                 "unreadable",
		"\n": "unreadable",
	} {
		a, joins := stubBootstrap(t, answer)
		out, err := bootstrap(bootstrapArgs...)
		if err == nil || !strings.Contains(err.Error(), want) || len(*joins) != 0 || a.ops()[len(a.ops())-1] != "ssm delete-parameter" || strings.Contains(err.Error()+out, "something else") {
			t.Fatalf("%q: %v %s %v", answer, err, out, a.ops())
		}
	}
	// No env file on the instance: exit 6.
	a, _ := stubBootstrap(t, "")
	a.status = "Failed"
	if _, err := bootstrap(bootstrapArgs...); err == nil || !strings.Contains(err.Error(), "COMPOSE_PROJECT_NAME") || a.ops()[len(a.ops())-1] != "ssm delete-parameter" {
		t.Fatal(err, a.ops())
	}
}

// Nothing runs before the inputs pass: AWS only, a server, a workspace
// name that is safe in the script.
func TestCloudBootstrapInputs(t *testing.T) {
	a, _ := stubBootstrap(t, "")
	for _, args := range [][]string{
		{"--ssh", "host", "--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test", "--workspace", "acme"},
		{"--ssh", "-oProxyCommand=x", "--server", "https://cb.example.test", "--workspace", "acme"},
		{"--server", "https://cb.example.test", "--workspace", "acme"},
		{"--instance", ec2ID, "--region", "us-east-1", "--workspace", "acme"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test", "--workspace", "a'cme"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test", "--workspace", "acme; rm"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "http://cb.example.test", "--workspace", "acme"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test", "--workspace", "acme", "extra"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test", "--workspace", "acme", "--email", "o'wner@example.test"},
		{"--instance", ec2ID, "--region", "us-east-1", "--server", "https://cb.example.test", "--workspace", "acme", "--email", "not-an-email"},
	} {
		if _, err := bootstrap(args...); err == nil {
			t.Fatal("took", args)
		}
	}
	if len(a.calls) != 0 {
		t.Fatal("ran aws:", a.ops())
	}
}

// stubExchange answers the --ssh bootstrap with answer, or err after
// writing stderr, and keeps each call.
func stubExchange(t *testing.T, answer, stderr string, err error) *[]remoteRun {
	t.Helper()
	runs := &[]remoteRun{}
	old := remoteExchange
	t.Cleanup(func() { remoteExchange = old })
	remoteExchange = func(_ context.Context, argv []string, stdin io.Reader, w io.Writer) ([]byte, error) {
		b, _ := io.ReadAll(stdin)
		*runs = append(*runs, remoteRun{argv: append(append([]string{}, argv...), "STDIN:"+string(b))})
		_, _ = io.WriteString(w, stderr)
		return []byte(answer), err
	}
	return runs
}

type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// Over plain SSH the code this process made goes only on ssh's stdin, to
// the fixed script, never in argv or the output; the owner joins with it.
func TestCloudBootstrapOverSSH(t *testing.T) {
	a, joins := stubBootstrap(t, "")
	runs := stubExchange(t, `invite {"invite_id":"inv_1","email":"owner@example.test","workspace":"acme","workspace_created":true,"expires_at":"2026-10-28T05:00:00Z"}`+"\n", "", nil)
	args := []string{"--ssh", "admin@cb.example.test", "--server", "https://cb.example.test", "--workspace", "acme", "--email", "owner@example.test"}
	out, err := bootstrap(args...)
	if err != nil || len(*runs) != 1 || len(*joins) != 1 || len(a.calls) != 0 {
		t.Fatalf("%v %s runs %d joins %d aws %v", err, out, len(*runs), len(*joins), a.ops())
	}
	code := (*joins)[0].Code
	argv := (*runs)[0].argv
	want := wantSSH("admin@cb.example.test", "env LC_ALL=C sudo -n sh -c '"+sshBootstrapScript("acme", "owner@example.test")+"'")
	if !reflect.DeepEqual(argv[:len(argv)-1], want) || argv[len(argv)-1] != "STDIN:"+code+"\n" || !strings.HasPrefix(code, "CBJ-") {
		t.Fatalf("argv %q", argv)
	}
	if strings.Contains(strings.Join(argv[:len(argv)-1], " ")+out, code) || (*joins)[0].Email != "owner@example.test" || !strings.Contains(out, `"device_id":"dev_1"`) {
		t.Fatalf("code leaked or wrong join: %s %+v", out, (*joins)[0])
	}
	// Each failure has its advice, and nothing joins.
	for name, c := range map[string]struct {
		answer, stderr string
		err            error
		want           string
	}{
		"refused":   {"refused clawbridge: the owner has 3 pending invites; revoke one first\n", "", nil, "(pending_invites)"},
		"no env":    {"", "", exitStatus(6), "COMPOSE_PROJECT_NAME"},
		"host key":  {"", "Host key verification failed.\n", exitStatus(255), "fingerprint"},
		"sudo":      {"", "sudo: a password is required\n", exitStatus(1), "passwordless sudo"},
		"unreached": {"", "ssh: connect to host cb.example.test port 22: Connection refused\n", exitStatus(255), "may or may not exist"},
	} {
		_, joins = stubBootstrap(t, "")
		stubExchange(t, c.answer, c.stderr, c.err)
		if _, err = bootstrap(args...); err == nil || !strings.Contains(err.Error(), c.want) || len(*joins) != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A lost join answer is settled by asking the server with the kept key
// before anyone asks for a new invite.
func TestCloudBootstrapProbesAnUncertainJoin(t *testing.T) {
	_, _ = stubBootstrap(t, `invite {"invite_id":"inv_1","email":"owner@example.test","workspace":"acme","workspace_created":true,"expires_at":"x"}`+"\n")
	joinDevice = func(context.Context, JoinParams) (JoinResult, error) {
		return JoinResult{}, &JoinUncertainError{DeviceID: "dev_9", KeyStore: "keyring", Server: "https://cb.example.test", Principal: "owner@example.test"}
	}
	var probed string
	probeJoin = func(_ context.Context, u *JoinUncertainError, workspace string, _ JoinParams) (JoinResult, error) {
		probed = u.DeviceID + "@" + workspace
		return JoinResult{DeviceID: u.DeviceID, Principal: u.Principal, Workspace: workspace}, nil
	}
	if out, err := bootstrap(bootstrapArgs...); err != nil || probed != "dev_9@acme" || !strings.Contains(out, `"device_id":"dev_9"`) {
		t.Fatal(err, probed, out)
	}
	probeJoin = func(_ context.Context, u *JoinUncertainError, _ string, _ JoinParams) (JoinResult, error) {
		return JoinResult{}, u
	}
	if _, err := bootstrap(bootstrapArgs...); err == nil || !strings.Contains(err.Error(), "dev_9") || !strings.Contains(err.Error(), "owner@example.test") {
		t.Fatal(err)
	}
}

// The script itself, run by sh with a fake aws and docker: the decrypted
// code reaches bootstrap-invite --code-stdin in a one-off api container of
// the env file's project, and only the invite line, or the operator's own
// error line, comes back.
func TestCloudBootstrapScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	const code = "CBJ-ABCD-EFGH-JKMN-PQRS"
	write(filepath.Join(bin, "aws"), "#!/bin/sh\ncase \"$*\" in *--with-decryption*) echo "+code+";; *) echo /clawbridge/x;; esac\n", 0o755)
	write(filepath.Join(bin, "docker"), `#!/bin/sh
printf "%s\n" "$*" > "$CB_DIR/docker.argv"
cat > "$CB_DIR/docker.stdin"
if [ -n "$CB_REFUSE" ]; then echo "Container started" >&2; echo "clawbridge: the owner has 3 pending invites; revoke one first" >&2; exit 1; fi
echo "noise before"
echo '{"invite_id":"inv_1","email":"owner@example.test"}'
`, 0o755)
	env := filepath.Join(dir, "clawbridge.env")
	write(env, "POSTGRES_PASSWORD=x\nCOMPOSE_PROJECT_NAME=clawbridge\n", 0o600)
	owner := ""
	run := func(envFile string, refuse bool) (string, int) {
		t.Helper()
		cmd := exec.Command("sh", "-c", ssmBootstrapScriptFor("/clawbridge/"+ec2ID+"/bootstrap-code", "us-east-1", "acme", owner, envFile, "/opt/c/compose.yaml"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "CB_DIR="+dir)
		if refuse {
			cmd.Env = append(cmd.Env, "CB_REFUSE=1")
		}
		out, err := cmd.Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	out, status := run(env, false)
	argv, _ := os.ReadFile(filepath.Join(dir, "docker.argv"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "docker.stdin"))
	if status != 0 || out != `invite {"invite_id":"inv_1","email":"owner@example.test"}`+"\n" || strings.TrimSpace(string(stdin)) != code ||
		strings.TrimSpace(string(argv)) != "compose --env-file "+env+" --project-name clawbridge -f /opt/c/compose.yaml run --rm --no-deps -T api operator bootstrap-invite --workspace acme --code-stdin" {
		t.Fatalf("%d %q argv %q stdin %q", status, out, argv, stdin)
	}
	owner = "owner@example.test"
	if _, status = run(env, false); status != 0 {
		t.Fatal(status)
	}
	if argv, _ = os.ReadFile(filepath.Join(dir, "docker.argv")); !strings.HasSuffix(strings.TrimSpace(string(argv)), "--workspace acme --expect-owner owner@example.test --code-stdin") {
		t.Fatalf("--email: %q", argv)
	}
	if out, status = run(env, true); status != 0 || out != "refused clawbridge: the owner has 3 pending invites; revoke one first\n" {
		t.Fatalf("refused: %d %q", status, out)
	}
	write(env, "POSTGRES_PASSWORD=x\n", 0o600)
	if _, status = run(env, false); status != 6 {
		t.Fatal("no project:", status)
	}
	if strings.Contains(ssmBootstrapScript("n", "us-east-1", "acme", "o.w+n-er@example.test"), "'") {
		t.Fatal("the script travels inside single quotes")
	}
}

// The real probe: a join whose answer was lost, then a whoami signed with
// the kept key; the device answers as its person, so the profile JoinDevice
// would have written is written. A device the server doesn't know leaves
// the key and the uncertainty.
func TestProbeUncertainJoinWritesTheProfile(t *testing.T) {
	f := newJoinFixture(t)
	params := JoinParams{Server: f.url, Email: "ada@example.com", Code: "CBJ-4K7P-WQ9D-2MXH-T6RB", HTTPClient: &http.Client{Transport: loseJoinAnswer{}}}
	_, err := JoinDevice(context.Background(), params)
	var lost *JoinUncertainError
	if !errors.As(err, &lost) {
		t.Fatal(err)
	}
	// A device the server doesn't know: still uncertain, nothing written.
	f.store.mu.Lock()
	known := f.store.devices[lost.DeviceID]
	delete(f.store.devices, lost.DeviceID)
	f.store.mu.Unlock()
	if _, err = probeUncertainJoin(context.Background(), lost, "acme", params); !errors.Is(err, remote.ErrJoinUncertain) {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	f.store.devices[lost.DeviceID] = known
	f.store.mu.Unlock()
	home, _ := stateHome()
	path := filepath.Join(home, "config.toml")
	if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("wrote a profile for an unknown device")
	}
	// A server that answers as someone else proves nothing.
	other := *lost
	other.Principal = "bob@example.com"
	if _, err = probeUncertainJoin(context.Background(), &other, "acme", params); !errors.Is(err, remote.ErrJoinUncertain) {
		t.Fatal(err)
	}
	f.exec.requests, f.exec.ids = nil, nil
	joined, err := probeUncertainJoin(context.Background(), lost, "acme", params)
	if err != nil || joined.DeviceID != lost.DeviceID || joined.ConfigPath != path {
		t.Fatal(joined, err)
	}
	p := profile(t, path)
	want := map[string]any{"mode": "cloud", "server": f.url, "workspace": "acme", "principal": "ada@example.com", "credential_source": "device", "device_id": lost.DeviceID, "key_store": "keyring", "cache": filepath.Join(home, "config.cache.db")}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}
	if n := len(f.exec.requests); n != 1 || f.exec.requests[0].Operation != "whoami" || f.exec.ids[0].Subject != lost.DeviceID {
		t.Fatalf("%+v %+v", f.exec.ids, f.exec.requests)
	}
}

// The --ssh script, run by sh with a fake docker: the code on its stdin
// reaches bootstrap-invite --code-stdin, nothing else of stdin is read,
// and a deployment that pulls the published image adds compose.image.yaml.
func TestSSHBootstrapScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "docker"), "#!/bin/sh\nprintf \"%s\\n\" \"$*\" > \"$CB_DIR/docker.argv\"\ncat > \"$CB_DIR/docker.stdin\"\necho '{\"invite_id\":\"inv_1\",\"email\":\"owner@example.test\"}'\n")
	env := filepath.Join(dir, "clawbridge.env")
	const code = "CBJ-ABCD-EFGH-JKMN-PQRS"
	run := func(body string) (string, string, string) {
		t.Helper()
		write(env, body)
		cmd := exec.Command("sh", "-c", sshBootstrapScriptFor("acme", "", env, "/opt/c/deploy/compose.yaml"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "CB_DIR="+dir)
		cmd.Stdin = strings.NewReader(code + "\nsecond line never read\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "docker.argv"))
		stdin, _ := os.ReadFile(filepath.Join(dir, "docker.stdin"))
		return string(out), strings.TrimSpace(string(argv)), string(stdin)
	}
	out, argv, stdin := run("COMPOSE_PROJECT_NAME=clawbridge\n")
	if out != `invite {"invite_id":"inv_1","email":"owner@example.test"}`+"\n" || stdin != code+"\n" ||
		argv != "compose --env-file "+env+" --project-name clawbridge -f /opt/c/deploy/compose.yaml run --rm --no-deps -T api operator bootstrap-invite --workspace acme --code-stdin" {
		t.Fatalf("%q argv %q stdin %q", out, argv, stdin)
	}
	_, argv, _ = run("COMPOSE_PROJECT_NAME=clawbridge\nCLAWBRIDGE_IMAGE=ghcr.io/jkai-chen/clawbridge@sha256:" + strings.Repeat("ab", 32) + "\n")
	if !strings.Contains(argv, "-f /opt/c/deploy/compose.yaml -f /opt/c/deploy/compose.image.yaml run") {
		t.Fatalf("image: %q", argv)
	}
	if strings.Contains(sshBootstrapScript("acme", "o.w+n-er@example.test"), "'") {
		t.Fatal("the script travels inside single quotes")
	}
}
