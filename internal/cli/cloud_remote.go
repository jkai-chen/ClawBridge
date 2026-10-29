package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// cloudTarget is the user's own route to the server's shell: their SSH,
// gcloud over IAP, or an EC2 instance over SSM with their AWS CLI profile.
// ClawBridge holds no credential of its own.
type cloudTarget struct {
	host, vm, project, zone, instance, region, profile, account string
	// local is the server on this machine (deploy/local-host.sh), in localDir.
	local    bool
	localDir string
}

func bindTarget(f *flag.FlagSet, t *cloudTarget) {
	f.StringVar(&t.host, "ssh", "", "SSH host of the server (any VM)")
	f.StringVar(&t.vm, "vm", "", "Compute Engine VM name (with --project and --zone, over IAP)")
	f.StringVar(&t.project, "project", "", "Google Cloud project")
	f.StringVar(&t.zone, "zone", "", "Compute Engine zone")
	bindAWSTarget(f, t)
}

// bindAWSTarget binds the AWS route's flags only.
func bindAWSTarget(f *flag.FlagSet, t *cloudTarget) {
	f.StringVar(&t.instance, "instance", "", "EC2 instance ID (with --region, over SSM)")
	f.StringVar(&t.region, "region", "", "AWS region")
	f.StringVar(&t.profile, "profile", "", "AWS CLI profile (default "+defaultAWSProfile+")")
	f.StringVar(&t.account, "account", "", "AWS account ID the profile must be signed in to")
}

func (t cloudTarget) set() bool { return t != cloudTarget{} }

// remote says the server's shell is reachable: a host, a VM or an instance.
// --project (or --region) alone only names the backup bucket's project.
func (t cloudTarget) remote() bool { return t.host != "" || t.vm != "" || t.instance != "" }

// onAWS says the target is AWS: any of the AWS flags is set.
func (t cloudTarget) onAWS() bool {
	return t.instance != "" || t.region != "" || t.profile != "" || t.account != ""
}

// argv is the command that runs remoteCommand on the server. Every input is
// validated first; remoteCommand is always one of the fixed scripts.
func (t cloudTarget) argv(remoteCommand string) ([]string, error) {
	switch {
	case t.host != "" && t.vm == "" && t.project == "" && t.zone == "":
		if !sshHostPattern.MatchString(t.host) {
			return nil, errors.New("--ssh must be a host name or user@host")
		}
		return []string{"ssh", "-T", t.host, remoteCommand}, nil
	case t.host == "" && t.vm != "":
		if !vmPattern.MatchString(t.vm) || !projectPattern.MatchString(t.project) || !zonePattern.MatchString(t.zone) {
			return nil, errors.New("--vm, --project and --zone must all be valid Compute Engine names")
		}
		return []string{"gcloud", "compute", "ssh", t.vm, "--project", t.project, "--zone", t.zone, "--tunnel-through-iap", "--command", remoteCommand}, nil
	}
	return nil, errors.New("the server is reached with --ssh HOST, or --vm NAME --project P --zone Z")
}

// remoteOutput runs a fixed remote command and returns its output.
var remoteOutput = func(ctx context.Context, argv []string, stderr io.Writer) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &limitedWriter{w: &out, n: 64 << 10}, stderr
	err := cmd.Run()
	return out.Bytes(), err
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		_, _ = l.w.Write(p[:l.n])
		l.n = 0
		return len(p), nil
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// requiredSettings are the compose file's required (`:?`) entries; a test
// keeps the list equal to deploy/compose.yaml.
var requiredSettings = []string{"CLAWBRIDGE_BOOTSTRAP_OWNER", "CLAWBRIDGE_DOMAIN", "POSTGRES_PASSWORD"}

// googleSettings are Google sign-in's five entries: all or none. A
// deployment with Google sign-in on (CLAWBRIDGE_AUTH unset, google or
// google,device) needs all five, and serve refuses to start without them;
// a device-only one (CLAWBRIDGE_AUTH=device) needs none.
var googleSettings = []string{
	"CLAWBRIDGE_AUDIENCE", "CLAWBRIDGE_OAUTH_CLIENT_ID", "CLAWBRIDGE_OAUTH_CLIENT_SECRET",
	"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET",
}

// authSetting chooses the sign-in methods. Unset, compose passes google.
const authSetting = "CLAWBRIDGE_AUTH"

// appSettings are the Linear app's three entries: all or none.
var appSettings = []string{"CLAWBRIDGE_LINEAR_APP_CLIENT_ID", "CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET", "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"}

// composeServices are the services that must run; db must also be healthy.
var composeServices = []string{"api", "caddy", "db"}

// factsScript is the one fixed remote command the checks run (§4, ON-L3).
// It prints, per key, how many non-empty lines set it (a count, never a
// value), the sign-in mode (CLAWBRIDGE_AUTH's last value, without spaces
// or quotes as compose reads it, when it is one of the four valid ones,
// else "invalid": never anything else), then each
// of the deployment's compose service containers (not `compose run`
// one-offs) as "service state status". It reads the env file's project name on the server and
// prints nothing else from it.
func factsScript() string { return factsScriptFor(serverEnvFile) }

func factsScriptFor(path string) string { return factsScriptWith(path, true) }

// factsScriptWith is the facts script; the data-disk line is the remote
// server's, so a server on this machine leaves it out.
func factsScriptWith(path string, withMount bool) string {
	mount := `echo "mount $(findmnt -n -o SOURCE ` + dataMount + ` || echo none)"`
	if !withMount {
		mount = `true`
	}
	keys := append(append(append(append([]string{}, requiredSettings...), googleSettings...), appSettings...), authSetting)
	return strings.Join([]string{
		`f=` + path,
		mount,
		`if [ ! -r "$f" ]; then echo "env missing"; exit 0; fi`,
		`for k in ` + strings.Join(keys, " ") + `; do echo "count $k $(grep -c "^$k=." "$f")"; done`,
		`a=$(sed -n "s/^` + authSetting + `=//p" "$f" | tail -n 1 | tr -d " \r\042\047")`,
		`case "$a" in "") echo "auth";; google|device|google,device|device,google) echo "auth $a";; *) echo "auth invalid";; esac`,
		`p=$(sed -n "s/^COMPOSE_PROJECT_NAME=//p" "$f" | tail -n 1)`,
		`[ -n "$p" ] && docker ps --all --filter "label=com.docker.compose.project=$p" --filter "label=com.docker.compose.oneoff=False" --format "service {{.Label \"com.docker.compose.service\"}} {{.State}} {{.Status}}"`,
		`exit 0`,
	}, "\n")
}

// dataMount is where the database's data disk is mounted.
const dataMount = "/var/lib/clawbridge-postgres"

type remoteFacts struct {
	mount      string // the data mount's source device, or "none"
	envMissing bool
	counts     map[string]int
	auth       string              // CLAWBRIDGE_AUTH, or "" when unset
	services   map[string][]string // service: each container's "state status"
}

func parseFacts(out []byte) remoteFacts {
	f := remoteFacts{counts: map[string]int{}, services: map[string][]string{}}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "env" && fields[1] == "missing":
			f.envMissing = true
		case len(fields) == 2 && fields[0] == "auth":
			f.auth = fields[1]
		case len(fields) == 3 && fields[0] == "count":
			if n, err := strconv.Atoi(fields[2]); err == nil {
				f.counts[fields[1]] = n
			}
		case len(fields) == 2 && fields[0] == "mount":
			f.mount = fields[1]
		case len(fields) >= 3 && fields[0] == "service":
			f.services[fields[1]] = append(f.services[fields[1]], strings.Join(fields[2:], " "))
		}
	}
	return f
}

// facts runs the fixed script once per cloud check.
func (p *cloudProbe) facts(ctx context.Context) (remoteFacts, error) {
	if p.factsDone {
		return p.remote, p.remoteErr
	}
	p.factsDone = true
	if p.target.local {
		var out []byte
		script, err := localFactsScript(p.target.localDir)
		if err == nil {
			out, err = remoteOutput(ctx, []string{"sh", "-c", script}, p.stderr)
		}
		if err == nil {
			p.remote = parseFacts(out)
		}
		p.remoteErr = err
		return p.remote, err
	}
	tr, err := p.target.transport()
	if err == nil {
		var out []byte
		if out, err = tr.run(ctx, factsScript(), p.stderr); err == nil {
			p.remote = parseFacts(out)
		}
	}
	p.remoteErr = err
	return p.remote, err
}

const remoteAdvice = "the server's shell is unreachable; check SSH access (--ssh or --vm) and passwordless sudo"

// checkSettings and checkServices extend the server check when a target is
// given: the required settings are present once each, and the deployment's
// containers run.
func checkSettings(f remoteFacts) cloudResult {
	if f.envMissing {
		return failed("server", "env_missing", serverEnvFile+" is missing; write the private configuration")
	}
	var missing, twice, google, noGoogle []string
	if f.counts[authSetting] > 1 {
		twice = append(twice, authSetting)
	}
	for _, k := range requiredSettings {
		switch n := f.counts[k]; {
		case n == 0:
			missing = append(missing, k)
		case n > 1:
			twice = append(twice, k)
		}
	}
	for _, k := range googleSettings {
		switch n := f.counts[k]; {
		case n == 0:
			noGoogle = append(noGoogle, k)
		case n > 1:
			twice = append(twice, k)
		default:
			google = append(google, k)
		}
	}
	if len(google) > 0 && len(noGoogle) > 0 {
		return failed("server", "setting_missing", "Google sign-in needs all five settings; set in the env file: "+strings.Join(noGoogle, ", ")+" (or remove them all and set CLAWBRIDGE_AUTH=device)")
	}
	methods, ok := authModeMethods(f.auth)
	if !ok {
		return failed("server", "setting_invalid", authSetting+" must be google, device or google,device")
	}
	if len(google) == 0 && slices.Contains(methods, "google") {
		return failed("server", "setting_missing", "Google sign-in is on ("+authSetting+" is "+cmp.Or(f.auth, "unset, so google")+") but none of its five settings is set; set them all, or set "+authSetting+"=device for device sign-in only")
	}
	if len(missing) > 0 {
		return failed("server", "setting_missing", "set in the env file: "+strings.Join(missing, ", "))
	}
	if len(twice) > 0 {
		return failed("server", "setting_duplicate", "set once only in the env file: "+strings.Join(twice, ", "))
	}
	return cloudResult{Check: "server", Status: "pass"}
}

func checkServices(f remoteFacts) cloudResult { return checkServicesOf(f, composeServices) }

func checkServicesOf(f remoteFacts, services []string) cloudResult {
	var down []string
	for _, s := range services {
		// Every container of the service must be up: a crashed replica
		// is not hidden by a healthy one.
		states := f.services[s]
		healthy := len(states) > 0
		for _, state := range states {
			up := strings.HasPrefix(state, "running ") && !strings.Contains(state, "(unhealthy)") && !strings.Contains(state, "(health: starting)")
			if s == "db" {
				up = up && strings.Contains(state, "(healthy)")
			}
			healthy = healthy && up
		}
		if !healthy {
			down = append(down, s)
		}
	}
	if len(down) > 0 {
		return failed("server", "service_down", "not running and healthy: "+strings.Join(down, ", ")+"; run docker compose up -d and check its logs")
	}
	return cloudResult{Check: "server", Status: "pass"}
}

// cloudSession resolves the configured cloud profile's workspace and project
// for the checks that ask the server.
func (p *cloudProbe) cloudSession(ctx context.Context, check string) (cloudExecutor, string, string, *cloudResult) {
	fail := func(reason, advice string) (cloudExecutor, string, string, *cloudResult) {
		r := failed(check, reason, advice)
		return nil, "", "", &r
	}
	if p.configErr != nil {
		return fail("no_config", "run clawbridge setup --cloud first")
	}
	if p.config.Mode != "cloud" {
		return fail("not_cloud", "this profile is local; run clawbridge setup --cloud")
	}
	if p.server != "" && strings.TrimSuffix(p.server, "/") != strings.TrimSuffix(p.config.Server, "/") {
		return fail("other_server", "--server differs from the configured server; check the configured one")
	}
	client, err := newCloudExecutor(p.config)
	if err != nil {
		return fail("not_signed_in", "run clawbridge login")
	}
	workspace, project, err := client.ResolveProject(ctx, p.config.Workspace, p.config.Project, "")
	if err != nil {
		return fail("no_workspace", "run clawbridge setup --cloud to create the workspace and its first project")
	}
	return client, workspace, project, nil
}

// checkLinearApp: the three entries are set once each (over SSH, when a
// target is given), and the server reports the app configured and seeing
// every bound team.
func checkLinearApp(ctx context.Context, p *cloudProbe) cloudResult {
	if p.target.remote() {
		f, err := p.facts(ctx)
		if err != nil {
			return p.remoteFailure("linear-app", err)
		}
		var unset []string
		for _, k := range appSettings {
			if f.counts[k] != 1 {
				unset = append(unset, k)
			}
		}
		if len(unset) > 0 {
			return failed("linear-app", "app_settings_missing", "set once each with clawbridge cloud secret set (the token key on the VM): "+strings.Join(unset, ", "))
		}
	}
	client, workspace, project, fail := p.cloudSession(ctx, "linear-app")
	if fail != nil {
		return *fail
	}
	r, err := client.Execute(ctx, bus.Request{Operation: "linear_app_status", Workspace: workspace, Project: project, Args: map[string]any{}})
	var refused *remote.HTTPError
	if errors.As(err, &refused) && refused.Status == 400 {
		return failed("linear-app", "not_configured", "the server runs without the Linear app; set its three entries and restart the API")
	}
	m, _ := r.Data.(map[string]any)
	if err != nil || m == nil || m["configured"] != true {
		return failed("linear-app", "status_unreadable", "the server could not report the app; check its startup log, then retry")
	}
	if m["binding"] != nil {
		return failed("linear-app", "binding_unreadable", "run clawbridge linear binding, and pin it with clawbridge linear link")
	}
	teams, _ := m["teams_visible"].(map[string]any)
	var hidden []string
	for team, seen := range teams {
		if seen != true {
			hidden = append(hidden, team)
		}
	}
	if len(hidden) > 0 {
		sort.Strings(hidden)
		return failed("linear-app", "teams_hidden", fmt.Sprintf("a Linear admin adds the app to %d bound team(s): %s", len(hidden), strings.Join(hidden, ", ")))
	}
	result := cloudResult{Check: "linear-app", Status: "pass"}
	if !p.target.remote() && !p.target.local {
		result.Advice = "entries not counted: pass --ssh, --vm or --instance to check them too"
	}
	return result
}

// authModeMethods reads CLAWBRIDGE_AUTH as serve does: its sign-in methods,
// sorted; unset is compose's google.
func authModeMethods(mode string) ([]string, bool) {
	var methods []string
	for _, m := range strings.Split(cmp.Or(mode, "google"), ",") {
		if (m != "google" && m != "device") || slices.Contains(methods, m) {
			return nil, false
		}
		methods = append(methods, m)
	}
	slices.Sort(methods)
	return methods, true
}
