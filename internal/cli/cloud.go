package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// The own-server onboarding probes (live/ONBOARDING-DESIGN.md §4). A check
// passes, warns, or fails with a fixed reason code and one line of advice; it
// never reads a secret's value and never writes.

const cloudUsage = `clawbridge cloud check [infra|server|version|workspace|linear-app|backups|all] [--server URL]
                       [--client-config PATH] [--ssh HOST | --vm NAME --project P --zone Z] [--bucket NAME] [--json]
clawbridge cloud secret set KEY (--ssh HOST | --vm NAME --project P --zone Z) [--value-stdin]`

type cloudResult struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Advice string `json:"advice,omitempty"`
}

type cloudProbe struct {
	server, version string
	config          Config
	configErr       error
	target          cloudTarget
	bucket          string
	stderr          io.Writer
	factsDone       bool
	remote          remoteFacts
	remoteErr       error
}

// cloudChecks is the fixed list, in the order "all" runs it. The sign-in
// check joins it with the device sign-in release.
// With --ssh or --vm, server also counts the required settings and checks
// the containers, and linear-app counts the app's entries.
var cloudChecks = []struct {
	name string
	run  func(context.Context, *cloudProbe) cloudResult
}{
	{"infra", checkInfra},
	{"server", checkServer},
	{"version", checkVersion},
	{"workspace", checkWorkspace},
	{"linear-app", checkLinearApp},
	{"backups", checkBackups},
}

var cloudHTTP = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func runCloud(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := fmt.Fprintln(stdout, cloudUsage)
		return err
	}
	switch {
	case args[0] == "check":
		return runCloudCheck(ctx, args[1:], stdout, stderr)
	case len(args) >= 2 && args[0] == "secret" && args[1] == "set":
		return runCloudSecretSet(ctx, args[2:], stdout, stderr)
	}
	return errors.New("unknown cloud command; see clawbridge cloud --help")
}

func runCloudCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var server, clientConfig string
	var asJSON bool
	var target cloudTarget
	var bucket string
	f := flag.NewFlagSet("cloud check", flag.ContinueOnError)
	f.SetOutput(stderr)
	bindTarget(f, &target)
	f.StringVar(&bucket, "bucket", "", "backup bucket (for backups)")
	f.StringVar(&server, "server", "", "server URL (default: the configured server)")
	f.StringVar(&clientConfig, "client-config", "", "trusted client configuration")
	f.BoolVar(&asJSON, "json", false, "print JSON")
	ordered, err := interspersedFlags(f, args)
	if err == nil {
		err = f.Parse(ordered)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, name := range f.Args() {
		known := name == "all"
		for _, c := range cloudChecks {
			known = known || c.name == name
		}
		if !known {
			return fmt.Errorf("unknown check %q; checks: infra, server, version, workspace, linear-app, backups, all", name)
		}
		want[name] = true
	}
	if len(want) == 0 {
		want["all"] = true
	}
	// --project alone names the bucket's project for backups.
	if target.set() && target != (cloudTarget{project: target.project}) {
		if _, err = target.argv(""); err != nil {
			return err
		}
	}
	probe := &cloudProbe{target: target, bucket: bucket, stderr: stderr}
	probe.config, _, probe.configErr = resolveConfig(ctx, Config{ClientConfig: clientConfig, Server: server})
	probe.server = server
	if probe.server == "" && probe.configErr == nil {
		probe.server = probe.config.Server
	}
	var results []cloudResult
	for _, c := range cloudChecks {
		if want["all"] || want[c.name] {
			results = append(results, c.run(ctx, probe))
		}
	}
	if asJSON {
		err = json.NewEncoder(stdout).Encode(map[string]any{"source": "cloud_check", "data": results})
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tSTATUS\tREASON\tADVICE")
		for _, r := range results {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Check, r.Status, r.Reason, r.Advice)
		}
		err = tw.Flush()
	}
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Status == "fail" {
			return errors.New("cloud check failed")
		}
	}
	return nil
}

func failed(check, reason, advice string) cloudResult {
	return cloudResult{Check: check, Status: "fail", Reason: reason, Advice: advice}
}

// serverURL accepts HTTPS, or plain HTTP to a loopback address only.
func serverURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, false
	}
	if u.Scheme == "https" {
		return u, true
	}
	ip := net.ParseIP(u.Hostname())
	return u, u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}

// health fetches /healthz once per run; the server and version checks share it.
func (p *cloudProbe) health(ctx context.Context) (string, cloudResult) {
	if p.server == "" {
		return "", failed("server", "no_server", "pass --server https://YOUR-DOMAIN or run setup --cloud")
	}
	u, ok := serverURL(p.server)
	if !ok {
		return "", failed("server", "invalid_url", "use https://YOUR-DOMAIN (plain http only for localhost)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(u.String(), "/")+"/healthz", nil)
	if err != nil {
		return "", failed("server", "invalid_url", "use https://YOUR-DOMAIN (plain http only for localhost)")
	}
	resp, err := cloudHTTP.Do(req)
	if err != nil {
		return "", failed("server", transportReason(err), transportAdvice[transportReason(err)])
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", failed("server", "http_status", fmt.Sprintf("GET /healthz answered %d; check the reverse proxy and that the server runs", resp.StatusCode))
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) != nil || body.Status != "ok" {
		return "", failed("server", "not_clawbridge", "the URL answers but is not a ClawBridge server")
	}
	return body.Version, cloudResult{Check: "server", Status: "pass"}
}

var transportAdvice = map[string]string{
	"dns":         "the domain does not resolve; check its DNS record",
	"tls":         "the certificate is not valid for this domain; check TLS on the reverse proxy",
	"unreachable": "no answer on the port; check the VM, the firewall and the reverse proxy",
}

func transportReason(err error) string {
	var dns *net.DNSError
	var verify *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &verify), errors.As(err, &header), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid):
		return "tls"
	}
	return "unreachable"
}

const serverUnchecked = "settings and services not checked: pass --ssh or --vm to check them too"

func checkServer(ctx context.Context, p *cloudProbe) cloudResult {
	version, r := p.health(ctx)
	p.version = version
	if r.Status != "pass" {
		return r
	}
	if !p.target.set() {
		r.Advice = serverUnchecked
		return r
	}
	f, err := p.facts(ctx)
	if err != nil {
		return failed("server", "ssh_failed", remoteAdvice)
	}
	if r = checkSettings(f); r.Status != "pass" {
		return r
	}
	return checkServices(f)
}

func checkVersion(ctx context.Context, p *cloudProbe) cloudResult {
	if p.version == "" {
		version, r := p.health(ctx)
		if r.Status != "pass" {
			r.Check = "version"
			return r
		}
		p.version = version
	}
	client := buildinfo.Version
	switch {
	case p.version == "":
		return failed("version", "server_too_old", "the server predates version reporting; update it to "+client)
	case client == "dev" || p.version == "dev":
		return cloudResult{Check: "version", Status: "warn", Reason: "dev_build", Advice: "a development build: versions not compared (client " + client + ", server " + p.version + ")"}
	case p.version != client:
		return failed("version", "version_mismatch", "client "+client+", server "+p.version+": update the older one to the same release")
	}
	return cloudResult{Check: "version", Status: "pass"}
}

// checkWorkspace signs in with the configured cloud route and asks whoami:
// the user must own the workspace.
func checkWorkspace(ctx context.Context, p *cloudProbe) cloudResult {
	client, workspace, project, fail := p.cloudSession(ctx, "workspace")
	if fail != nil {
		return *fail
	}
	result, err := client.Execute(ctx, bus.Request{Operation: "whoami", Workspace: workspace, Project: project, Args: map[string]any{}})
	if err != nil {
		return failed("workspace", "whoami_failed", "run clawbridge login, then retry")
	}
	data, _ := result.Data.(map[string]any)
	w, _ := data["workspace"].(map[string]any)
	if principal, _ := data["principal"].(string); w == nil || principal == "" || w["owner_principal"] != principal {
		return failed("workspace", "not_owner", "you are signed in but do not own this workspace; sign in as its owner")
	}
	return cloudResult{Check: "workspace", Status: "pass"}
}

type cloudExecutor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
	ResolveProject(context.Context, string, string, string) (string, string, error)
}

var newCloudExecutor = func(c Config) (cloudExecutor, error) {
	return remote.New(remoteConfig(c))
}

// secretKeys are the only keys secret set writes. The value is the user's,
// read here and sent over their own SSH session; it never passes through an
// agent.
var secretKeys = map[string]bool{
	"CLAWBRIDGE_LINEAR_APP_CLIENT_ID":     true,
	"CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET": true,
}

const serverEnvFile = "/etc/clawbridge/clawbridge.env"

var (
	sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,252}$`)
	vmPattern      = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	projectPattern = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]$`)
	zonePattern    = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
)

var (
	cloudStdin    io.Reader = os.Stdin
	stdinTerminal           = func() bool { return term.IsTerminal(os.Stdin.Fd()) }
	readHidden              = func() ([]byte, error) { return term.ReadPassword(os.Stdin.Fd()) }
	runRemote               = func(ctx context.Context, argv []string, stdin io.Reader, stderr io.Writer) error {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, io.Discard, stderr
		return cmd.Run()
	}
)

// remoteSetScript replaces KEY's line in the env file, or in the regular file
// it links to, and installs it 0600, root-owned. KEY comes from secretKeys only; the value arrives on stdin and
// is never in argv, the environment or the output. It has no single quote,
// so it travels inside one.
func remoteSetScript(key, path string) string {
	return strings.Join([]string{
		`set -eu`,
		`umask 077`,
		`f=` + path,
		// A deployment may link the env file elsewhere: write the target.
		`if [ -L "$f" ]; then f=$(readlink -f "$f"); fi`,
		`if [ -e "$f" ] && [ ! -f "$f" ]; then exit 4; fi`,
		`IFS= read -r v || [ -n "$v" ]`,
		`[ -n "$v" ]`,
		`mkdir -p "$(dirname "$f")"`,
		`t=$(mktemp "$f.XXXXXX")`,
		`trap "rm -f \"\$t\"" EXIT`,
		`if [ -f "$f" ]; then grep -v "^` + key + `=" "$f" > "$t" || [ $? -eq 1 ]; fi`,
		`printf "%s=%s\n" ` + key + ` "$v" >> "$t"`,
		`chown 0:0 "$t"`,
		`chmod 0600 "$t"`,
		`mv -f "$t" "$f"`,
	}, "\n")
}

// secretValue rejects what the env file or the remote read cannot carry
// verbatim: whitespace, quotes, $, #, backslash and backtick.
func secretValue(raw []byte) (string, error) {
	v := strings.TrimRight(string(raw), "\r\n")
	if v == "" {
		return "", errors.New("empty value")
	}
	if len(v) > 4096 {
		return "", errors.New("value longer than 4096 bytes")
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x21 || c > 0x7e || strings.IndexByte("\"'$#\\`", c) >= 0 {
			return "", errors.New("value has an unsupported character (whitespace, quote, $, #, backslash or backtick)")
		}
	}
	return v, nil
}

func runCloudSecretSet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var target cloudTarget
	var fromStdin bool
	f := flag.NewFlagSet("cloud secret set", flag.ContinueOnError)
	f.SetOutput(stderr)
	bindTarget(f, &target)
	f.BoolVar(&fromStdin, "value-stdin", false, "read the value from a pipe instead of a hidden prompt")
	ordered, err := interspersedFlags(f, args)
	if err == nil {
		err = f.Parse(ordered)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if f.NArg() != 1 || !secretKeys[f.Arg(0)] {
		return errors.New("secret set takes one KEY: CLAWBRIDGE_LINEAR_APP_CLIENT_ID or CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET")
	}
	key := f.Arg(0)
	argv, err := target.argv(`sudo -n sh -c '` + remoteSetScript(key, serverEnvFile) + `'`)
	if err != nil {
		return err
	}
	var raw []byte
	switch {
	case fromStdin:
		raw, err = io.ReadAll(io.LimitReader(bufio.NewReader(cloudStdin), 4098))
		if err == nil && strings.Count(strings.TrimRight(string(raw), "\r\n"), "\n") > 0 {
			err = errors.New("the value must be one line")
		}
	case stdinTerminal():
		fmt.Fprintf(stderr, "Value for %s (hidden): ", key)
		raw, err = readHidden()
		fmt.Fprintln(stderr)
	default:
		err = errors.New("stdin is not a terminal; pass --value-stdin to read the value from a pipe")
	}
	if err != nil {
		return err
	}
	value, err := secretValue(raw)
	if err != nil {
		return err
	}
	if err = runRemote(ctx, argv, strings.NewReader(value+"\n"), stderr); err != nil {
		return errors.New("the remote write failed; check SSH access and passwordless sudo on the server (nothing was printed)")
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"source": "cloud_secret", "data": map[string]any{"key": key, "set": true}})
}
