package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// sshOptions come before the host on every --ssh command. On the command
// line they win over the user's ~/.ssh/config, which still supplies the
// identity and any ProxyJump: never a prompt, never a host key accepted
// for the user, and no agent, X11, port forwarding or local command
// reaching a host where commands run with sudo.
var sshOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=15",
	"-o", "StrictHostKeyChecking=yes",
	"-o", "ForwardAgent=no",
	"-o", "ForwardX11=no",
	"-o", "ClearAllForwardings=yes",
	"-o", "PermitLocalCommand=no",
	"-o", "RemoteCommand=none",
}

var (
	// errHostKey: ssh refused the host's key, unknown or changed.
	errHostKey = errors.New("ssh host key not verified")
	// errSudoPassword: sudo -n needs a password.
	errSudoPassword = errors.New("sudo needs a password")
)

// sshFailure names what a failed ssh run's stderr says, from fixed
// phrases only; anything else is returned as it was.
func sshFailure(err error, stderr []byte) error {
	switch s := string(stderr); {
	case strings.Contains(s, "Host key verification failed"), strings.Contains(s, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return errHostKey
	case strings.Contains(s, "sudo: a password is required"), strings.Contains(s, "sudo: a terminal is required"):
		return errSudoPassword
	}
	return err
}

// tail keeps the last 4 KiB written to it, to read ssh's refusal from.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4<<10 {
		t.b = t.b[len(t.b)-4<<10:]
	}
	return len(p), nil
}

// teeStderr passes ssh's stderr on to the user and keeps its tail.
func teeStderr(stderr io.Writer) (io.Writer, *tail) {
	t := &tail{}
	if stderr == nil {
		return t, t
	}
	return io.MultiWriter(stderr, t), t
}

// hostFactsScript is the fixed script infra runs on an SSH host, where no
// cloud API can answer (ANY-VM-PLAN §2). It prints one fact per line and
// never a value from the env file: for the opt-out, a count.
func hostFactsScript() string { return hostFactsScriptFor(serverEnvFile, dataMount) }

func hostFactsScriptFor(envFile, data string) string {
	return strings.Join([]string{
		`f=` + envFile,
		`d=` + data,
		`echo "platform $(uname -s) $(uname -m)"`,
		`if command -v docker >/dev/null 2>&1; then`,
		`  echo "docker $(docker version --format "{{.Server.Version}}" 2>/dev/null || echo none)"`,
		`  echo "compose $(docker compose version --short 2>/dev/null || echo none)"`,
		`else echo "docker none"; echo "compose none"; fi`,
		`echo "mount $(findmnt -n -o SOURCE "$d" || echo none)"`,
		`p=$d; while [ ! -d "$p" ]; do p=$(dirname "$p"); done`,
		`echo "free $(df -Pk "$p" | tail -n 1 | tr -s " " | cut -d " " -f 4)"`,
		`if command -v timedatectl >/dev/null 2>&1; then echo "ntp $(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo unknown)"; else echo "ntp unknown"; fi`,
		`if [ -e "$f" ]; then echo "env $(stat -L -c %a:%U "$f")"; echo "rootdisk $(grep -c "^CLAWBRIDGE_DATA_ON_ROOT_DISK=1$" "$f")"; else echo "env absent"; fi`,
		`ip -o addr show scope global 2>/dev/null | while read -r _ _ _ a _; do echo "addr ${a%/*}"; done`,
		`exit 0`,
	}, "\n")
}

// hostFacts are the parsed facts; a line that doesn't parse strictly is
// left out, as if the fact were missing (remote output is untrusted).
type hostFacts struct {
	arch     string // amd64 or arm64; "" when unsupported
	docker   bool
	compose  [2]int // major, minor; zero when missing
	mounted  bool
	freeKiB  int64
	ntp      string // yes, no or unknown
	env      string // "absent", "600:root", or another mode:owner
	rootDisk bool
	addrs    []net.IP
}

var (
	factVersion = regexp.MustCompile(`^v?([0-9]{1,4})\.([0-9]{1,4})(\.[0-9]{1,4})?([-+][0-9A-Za-z.-]{0,40})?$`)
	factMode    = regexp.MustCompile(`^[0-7]{3,4}:[a-z_][a-z0-9_-]{0,31}$`)
	factDevice  = regexp.MustCompile(`^[A-Za-z0-9/_.:\[\]-]{1,200}$`)
)

func parseHostFacts(out []byte) hostFacts {
	var h hostFacts
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "platform" && len(f) == 3 && f[1] == "Linux":
			switch f[2] {
			case "x86_64", "amd64":
				h.arch = "amd64"
			case "aarch64", "arm64":
				h.arch = "arm64"
			}
		case f[0] == "docker" && len(f) == 2:
			h.docker = factVersion.MatchString(f[1])
		case f[0] == "compose" && len(f) == 2:
			if m := factVersion.FindStringSubmatch(f[1]); m != nil {
				h.compose[0], _ = strconv.Atoi(m[1])
				h.compose[1], _ = strconv.Atoi(m[2])
			}
		case f[0] == "mount" && len(f) == 2:
			h.mounted = f[1] != "none" && factDevice.MatchString(f[1])
		case f[0] == "free" && len(f) == 2:
			h.freeKiB, _ = strconv.ParseInt(f[1], 10, 64)
		case f[0] == "ntp" && len(f) == 2 && (f[1] == "yes" || f[1] == "no" || f[1] == "unknown"):
			h.ntp = f[1]
		case f[0] == "env" && len(f) == 2 && (f[1] == "absent" || factMode.MatchString(f[1])):
			h.env = f[1]
		case f[0] == "rootdisk" && len(f) == 2:
			h.rootDisk = f[1] == "1"
		case f[0] == "addr" && len(f) == 2:
			if ip := net.ParseIP(f[1]); ip != nil {
				h.addrs = append(h.addrs, ip)
			}
		}
	}
	return h
}

// minFreeKiB is the data path's least free space: 10 GiB.
const minFreeKiB = 10 << 20

const (
	dockerDocs       = "install Docker Engine with the Compose plugin: https://docs.docker.com/engine/install/"
	rootDiskOptOut   = "CLAWBRIDGE_DATA_ON_ROOT_DISK=1"
	rootDiskWarning  = "the database is on the root disk (" + rootDiskOptOut + "): reinstalling the OS means a restore from the off-host backups"
	dnsAdvisory      = "the domain's addresses are none of this host's own (normal behind 1:1 NAT); check its A record is this host's public address"
	dnsAdvisoryShort = "dns_not_this_host"
	hostKeyAdvice    = "ssh did not verify the host's key: connect once with ssh HOST, compare the fingerprint with your provider's console, then run this again"
	sudoAdvice       = "sudo asked for a password: give this user passwordless sudo (sudo -n) on the server"
)

// sshInfra is infra for a host reached over plain SSH: what the host itself
// can show, with no cloud API. A part that can't be proved here but isn't
// wrong is a warning: the database on the root disk by the owner's stated
// choice, or a domain whose addresses aren't on the host's interfaces.
func sshInfra(ctx context.Context, p *cloudProbe) cloudResult {
	u, ok := serverURL(p.server)
	if !ok {
		return failed("infra", "no_server", "pass --server https://YOUR-DOMAIN or run setup --cloud")
	}
	tr, err := p.target.transport()
	if err != nil {
		return failed("infra", "needs_host", "pass --ssh HOST")
	}
	out, err := tr.run(ctx, hostFactsScript(), p.stderr)
	if err != nil {
		return p.remoteFailure("infra", err)
	}
	h := parseHostFacts(out)
	switch {
	case h.arch == "":
		return failed("infra", "unsupported_platform", "the server image runs on 64-bit Linux, amd64 or arm64")
	case !h.docker:
		return failed("infra", "docker_missing", dockerDocs)
	case h.compose[0] < 2 || h.compose[0] == 2 && h.compose[1] < 24:
		return failed("infra", "compose_too_old", "Docker Compose v2.24 or later is needed; "+dockerDocs)
	case h.env != "" && h.env != "absent" && h.env != "600:root":
		return failed("infra", "env_file_mode", serverEnvFile+" must be mode 600, owned by root")
	case !h.mounted && !h.rootDisk:
		return failed("infra", "data_not_mounted", "mount a separate volume at "+dataMount+" (and in /etc/fstab), or state "+rootDiskOptOut+" in "+serverEnvFile+" to keep the database on the root disk")
	case h.freeKiB < minFreeKiB:
		return failed("infra", "data_disk_full", "the data path needs at least 10 GiB free")
	case h.ntp == "no":
		return failed("infra", "clock_unsynced", "turn on time synchronisation (timedatectl set-ntp true): signatures allow 5 minutes of skew")
	}
	domain := u.Hostname()
	addrs, _ := lookupHost(ctx, domain)
	if len(addrs) == 0 {
		return failed("infra", "dns_unresolved", domain+" must have an A or AAAA record for this host")
	}
	// Last: nothing answers on 443 until the server is installed.
	if dialTCP(ctx, net.JoinHostPort(domain, "443")) != nil {
		return failed("infra", "port_closed", "TCP 443 does not answer; open 80 and 443 in the firewall and start the server")
	}
	var reasons, advice []string
	if !h.mounted {
		reasons, advice = append(reasons, "data_on_root_disk"), append(advice, rootDiskWarning)
	}
	if !resolvesHere(addrs, h.addrs) {
		reasons, advice = append(reasons, dnsAdvisoryShort), append(advice, dnsAdvisory)
	}
	if h.ntp == "unknown" {
		reasons, advice = append(reasons, "clock_unknown"), append(advice, "time synchronisation could not be read; check the clock is NTP-synchronised")
	}
	if len(reasons) > 0 {
		return cloudResult{Check: "infra", Status: "warn", Reason: strings.Join(reasons, ","), Advice: strings.Join(advice, "; ")}
	}
	return cloudResult{Check: "infra", Status: "pass"}
}

// resolvesHere says one of the domain's addresses is on the host.
func resolvesHere(domain []string, host []net.IP) bool {
	for _, a := range domain {
		ip := net.ParseIP(a)
		for _, h := range host {
			if ip != nil && ip.Equal(h) {
				return true
			}
		}
	}
	return false
}

// sshRun runs argv for the SSH route, naming a refused host key or a sudo
// that wants a password.
func sshRun(ctx context.Context, argv []string, stdin io.Reader, stderr io.Writer) ([]byte, error) {
	w, t := teeStderr(stderr)
	if stdin == nil {
		out, err := remoteOutput(ctx, argv, w)
		if err != nil {
			err = sshFailure(err, t.b)
		}
		return out, err
	}
	err := runRemote(ctx, argv, stdin, w)
	if err != nil {
		err = sshFailure(err, t.b)
	}
	return nil, err
}
