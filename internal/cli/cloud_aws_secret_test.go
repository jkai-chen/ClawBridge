package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const ssmSecretKey = "CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET"

// fakeAWSScript is an aws CLI that records each call's argv, environment and
// stdin under $CB_FAKE_AWS_LOG, and answers the calls secret set makes.
const fakeAWSScript = `#!/bin/sh
d="$CB_FAKE_AWS_LOG"
n=$(cat "$d/.n" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$d/.n"
printf '%s\n' "$@" > "$d/$n.argv"
env > "$d/$n.env"
cat > "$d/$n.stdin"
case "$1 $2" in
"sts get-caller-identity") echo '{"Account":"123456789012"}' ;;
"ssm put-parameter") echo '{"Version":1}' ;;
"ssm send-command") echo '{"Command":{"CommandId":"cmd-1"}}' ;;
"ssm get-command-invocation") echo '{"Status":"Success","StandardOutputContent":""}' ;;
"ssm delete-parameter") [ -z "$CB_FAKE_AWS_FAIL_DELETE" ] || exit 254 ;;
*) exit 2 ;;
esac
`

func fakeAWS(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake aws CLI is a POSIX shell script")
	}
	bin, log := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fakeAWSScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CB_FAKE_AWS_LOG", log)
	return log
}

// The value travels only on put-parameter's stdin: never in any argv, any
// child's environment, send-command's request, or ClawBridge's output; and
// the parameter is deleted last.
func TestCloudSecretSetOverSSMSendsTheValueOnlyToParameterStore(t *testing.T) {
	log := fakeAWS(t)
	const value = "lin_synthetic-ssm-secret_0123XYZ"
	stubSecretIO(t, value, false, nil)
	out, err := secretSet(ssmSecretKey, "--instance", ec2ID, "--region", "us-east-1", "--account", "123456789012", "--value-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, value) || !strings.Contains(out, `"set":true`) {
		t.Fatalf("output: %s", out)
	}
	n, _ := os.ReadFile(filepath.Join(log, ".n"))
	calls := strings.TrimSpace(string(n))
	var ops []string
	for i := 1; ; i++ {
		argv, err := os.ReadFile(filepath.Join(log, strconv.Itoa(i)+".argv"))
		if err != nil {
			break
		}
		lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
		op := lines[0] + " " + lines[1]
		ops = append(ops, op)
		env, _ := os.ReadFile(filepath.Join(log, strconv.Itoa(i)+".env"))
		stdin, _ := os.ReadFile(filepath.Join(log, strconv.Itoa(i)+".stdin"))
		if strings.Contains(string(argv), value) || strings.Contains(string(env), value) {
			t.Fatalf("%s: the value is in argv or the environment", op)
		}
		if strings.Contains(string(stdin), value) != (op == "ssm put-parameter") {
			t.Fatalf("%s: stdin carries the value: %v", op, strings.Contains(string(stdin), value))
		}
		switch op {
		case "ssm put-parameter":
			var req map[string]any
			if json.Unmarshal(stdin, &req) != nil || req["Name"] != "/clawbridge/"+ec2ID+"/"+ssmSecretKey || req["Type"] != "SecureString" {
				t.Fatalf("put-parameter request: %s", stdin)
			}
		case "ssm send-command":
			if !strings.Contains(string(stdin), "get-parameter --with-decryption --name /clawbridge/"+ec2ID+"/"+ssmSecretKey) {
				t.Fatalf("send-command request: %s", stdin)
			}
		case "ssm delete-parameter":
			if !strings.Contains(string(argv), "/clawbridge/"+ec2ID+"/"+ssmSecretKey) {
				t.Fatalf("delete-parameter argv: %s", argv)
			}
		}
	}
	want := []string{"sts get-caller-identity", "ssm put-parameter", "ssm send-command", "ssm get-command-invocation", "ssm delete-parameter"}
	if strings.Join(ops, ",") != strings.Join(want, ",") || calls != strconv.Itoa(len(want)) {
		t.Fatalf("calls: %v", ops)
	}
}

// The parameter is deleted on every path: a refused put, a refused or failed
// or unfinished command, and a cancelled run.
func TestCloudSecretSetOverSSMDeletesTheParameterOnEveryPath(t *testing.T) {
	for name, c := range map[string]struct {
		edit   func(*awsStub)
		cancel bool
	}{
		"written":          {},
		"put refused":      {edit: func(a *awsStub) { a.err = map[string]error{"ssm put-parameter": errors.New("x")} }},
		"command refused":  {edit: func(a *awsStub) { a.out["ssm send-command"] = `{}` }},
		"command failed":   {edit: func(a *awsStub) { a.out["ssm get-command-invocation"] = `{"Status":"Failed"}` }},
		"command too slow": {edit: func(a *awsStub) { a.out["ssm get-command-invocation"] = `{"Status":"InProgress"}`; ssmWait = 0 }},
		"cancelled":        {cancel: true},
	} {
		a := healthyAWS()
		a.out["ssm put-parameter"] = `{"Version":1}`
		a.out["ssm delete-parameter"] = `{}`
		a.out["ssm get-command-invocation"] = `{"Status":"Success","StandardOutputContent":""}`
		stubAWS(t, a, nil, nil)
		if c.edit != nil {
			c.edit(a)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if c.cancel {
			cancel()
			inner := awsCLI
			awsCLI = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
				if ctx.Err() != nil {
					a.calls = append(a.calls, argv)
					return nil, ctx.Err()
				}
				return inner(ctx, argv, stdin)
			}
		}
		tr, _ := cloudTarget{instance: ec2ID, region: "us-east-1"}.transport()
		err := tr.setSecret(ctx, ssmSecretKey, "synthetic-value", nil)
		cancel()
		if (name == "written") != (err == nil) {
			t.Fatalf("%s: %v", name, err)
		}
		deletes := 0
		for _, argv := range a.calls {
			if argv[2] == "delete-parameter" {
				deletes++
			}
		}
		if deletes != 1 || a.calls[len(a.calls)-1][2] != "delete-parameter" {
			t.Fatalf("%s: %d deletes, last %v", name, deletes, a.calls[len(a.calls)-1])
		}
	}
}

// A delete that fails is reported, by the parameter's name and never its value.
func TestCloudSecretSetOverSSMReportsAParameterLeftBehind(t *testing.T) {
	fakeAWS(t)
	t.Setenv("CB_FAKE_AWS_FAIL_DELETE", "1")
	const value = "lin_synthetic-left-behind_456"
	stubSecretIO(t, value, false, nil)
	_, err := secretSet(ssmSecretKey, "--instance", ec2ID, "--region", "us-east-1", "--value-stdin")
	if err == nil || !strings.Contains(err.Error(), "parameter left behind: /clawbridge/"+ec2ID+"/"+ssmSecretKey) ||
		!strings.Contains(err.Error(), "nothing was printed") || strings.Contains(err.Error(), value) {
		t.Fatalf("%v", err)
	}
}

// A profile signed in to another account is refused before the value is read.
func TestCloudSecretSetOverSSMChecksTheAccountFirst(t *testing.T) {
	a := &awsStub{out: map[string]string{"sts get-caller-identity": `{"Account":"999999999999"}`}}
	stubAWS(t, a, nil, nil)
	read := false
	stubSecretIO(t, "v", true, nil)
	readHidden = func() ([]byte, error) { read = true; return []byte("v"), nil }
	_, err := secretSet(ssmSecretKey, "--instance", ec2ID, "--region", "us-east-1", "--account", "123456789012")
	if !errors.Is(err, errCloudAuth) || read || len(a.calls) != 1 {
		t.Fatalf("%v, value read: %v, calls: %v", err, read, a.calls)
	}
}

// On the instance: the parameter's value goes through a pipe into
// remoteSetScript; a failed read writes nothing.
func TestSSMSetScriptRunsOnTheInstance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on the Linux server")
	}
	name := secretParameter(ec2ID, ssmSecretKey)
	script := ssmSetScript(name, "us-east-1", ssmSecretKey)
	if strings.Contains(script, "'") {
		t.Fatal("the script has a single quote")
	}
	path := filepath.Join(t.TempDir(), "clawbridge.env")
	local := strings.Replace(strings.ReplaceAll(script, serverEnvFile, path), `chown 0:0 "$t"`, ":", 1)
	bin := t.TempDir()
	run := func(fake string) error {
		if err := os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\n"+fake+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", local)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		return cmd.Run()
	}
	if err := run(`[ "$*" = "ssm get-parameter --with-decryption --name ` + name + ` --region us-east-1 --query Parameter.Value --output text" ] && echo fromstore`); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != ssmSecretKey+"=fromstore\n" {
		t.Fatalf("env file: %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if err := run(`exit 255`); err == nil {
		t.Fatal("a failed read was accepted")
	}
	if got, _ := os.ReadFile(path); string(got) != ssmSecretKey+"=fromstore\n" {
		t.Fatalf("a failed read changed the file: %q", got)
	}
}
