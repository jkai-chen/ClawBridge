// Package agentd is clawbridge-agentd, the launcher that runs one dispatcher
// agent container per key-holder (DISPATCHER-AGENT-DESIGN §11.4, §11.8).
//
// Its Docker access is root-equivalent, so it is kept small:
//   - The container spec is compiled in. The server only says which owner refs
//     should run, never how.
//   - agentd talks to a separate rootless dockerd. A validating socket proxy
//     (Proxy) in front of that daemon allows a fixed set of endpoints and
//     refuses any create body that is not exactly this spec [AG-M1].
package agentd

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

var (
	ownerRefPattern = regexp.MustCompile(`^p_[a-z0-9]{8,32}$`)
	// imagePattern is an image pinned by digest; a tag alone is refused.
	imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$`)
)

const (
	containerPrefix = "clawbridge-agent-"
	networkPrefix   = "cbnet-"
	agentMemory     = 160 << 20
)

// revisionLabel carries the owner's config revision the container was
// started for, so agentd sees a rotation even after it restarts itself.
const revisionLabel = "clawbridge.config_revision"

var revisionPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// parseRevision reads a revisionLabel value: a positive decimal.
func parseRevision(s string) (int64, bool) {
	if !revisionPattern.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// validOwnerRef reports whether ref is an opaque owner reference.
func validOwnerRef(ref string) bool { return ownerRefPattern.MatchString(ref) }

func containerName(ref string) string { return containerPrefix + ref }
func networkName(ref string) string   { return networkPrefix + ref }

// refFromName returns the owner ref in a container or network name made by
// containerName or networkName, or false.
func refFromName(name, prefix string) (string, bool) {
	ref, ok := strings.CutPrefix(name, prefix)
	return ref, ok && validOwnerRef(ref)
}

// createSpec is the body of POST /containers/create. It has a field for
// everything agentd sets and nothing else, so a volume, a device or an added
// capability can't be expressed; the proxy decodes into it with unknown
// fields refused.
type createSpec struct {
	Image       string
	User        string
	Env         []string
	Labels      map[string]string
	AttachStdin bool
	OpenStdin   bool
	StdinOnce   bool
	HostConfig  hostConfig
}

type hostConfig struct {
	ReadonlyRootfs bool
	Tmpfs          map[string]string
	CapDrop        []string
	SecurityOpt    []string
	PidsLimit      int64
	Memory         int64
	MemorySwap     int64
	NetworkMode    string
	RestartPolicy  restartPolicy
}

type restartPolicy struct{ Name string }

// agentSpec is the fixed container spec for one owner (§5.3): read-only root,
// a small tmpfs, the image's non-root user, no capabilities, no new
// privileges, 64 pids, 160 MiB with no swap, the owner's own internal
// network, two env variables (the server and the egress proxy), the config
// revision as its one label, and stdin open once for the agent credential.
// agentd removes exited containers itself, so Docker never restarts one.
func agentSpec(image, serverURL, ref string, revision int64) createSpec {
	return createSpec{
		Image:       image,
		User:        "node",
		Env:         []string{"CLAWBRIDGE_SERVER_URL=" + serverURL, "CLAWBRIDGE_EGRESS_PROXY=" + egressProxyURL},
		Labels:      map[string]string{revisionLabel: strconv.FormatInt(revision, 10)},
		AttachStdin: true,
		OpenStdin:   true,
		StdinOnce:   true,
		HostConfig: hostConfig{
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": "rw,noexec,nosuid,size=8m"},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			PidsLimit:      64,
			Memory:         agentMemory,
			MemorySwap:     agentMemory,
			NetworkMode:    networkName(ref),
			RestartPolicy:  restartPolicy{Name: "no"},
		},
	}
}

// networkSpec is the body of POST /networks/create.
type networkSpec struct {
	Name     string
	Driver   string
	Internal bool
	IPAM     ipamSpec
}

type ipamSpec struct {
	Driver string
	Config []ipamConfig
}

type ipamConfig struct{ Subnet string }

// Each owner's network has a fixed subnet from this pool, so the egress
// proxy can tell owners apart by source address (§5.3). agentd allocates
// them; the proxy only checks the shape, and the daemon refuses overlaps.
var (
	subnetPool = netip.MustParsePrefix("172.30.0.0/16")
	// egressSubnet is the egress proxy's own outbound network, outside the
	// pool.
	egressSubnet = netip.MustParsePrefix("172.31.255.0/28")
)

const subnetBits = 28

// validAgentSubnet reports whether subnet is a /28 of the pool.
func validAgentSubnet(subnet string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(subnet)
	ok := err == nil && p.Bits() == subnetBits && p == p.Masked() && subnetPool.Contains(p.Addr())
	return p, ok
}

// agentNetwork is an owner's network: an internal bridge, with no route
// out, on its fixed subnet. The egress proxy is its only other member.
func agentNetwork(ref string, subnet netip.Prefix) networkSpec {
	return networkSpec{Name: networkName(ref), Driver: "bridge", Internal: true,
		IPAM: ipamSpec{Driver: "default", Config: []ipamConfig{{Subnet: subnet.String()}}}}
}

// egressProxyURL is how an agent reaches the egress proxy on its own
// network: Docker's DNS answers the container's name there.
const egressProxyURL = "http://" + egressName + ":" + egressPort

const (
	egressName    = "clawbridge-egress"
	egressNetName = "clawbridge-egress-net"
	egressPort    = "3128"
	// policyMount is where the egress container reads the policy.
	policyMount = "/policy"
	policyFile  = "egress.json"
)

// egressNetwork is the one network with a route out: the egress proxy's.
func egressNetSpec() networkSpec {
	return networkSpec{Name: egressNetName, Driver: "bridge", Internal: false,
		IPAM: ipamSpec{Driver: "default", Config: []ipamConfig{{Subnet: egressSubnet.String()}}}}
}

// egressCreate is the body of POST /containers/create for the egress proxy.
// Like createSpec it has a field for everything set and nothing else.
type egressCreate struct {
	Image      string
	User       string
	Cmd        []string
	HostConfig egressHostConfig
}

type egressHostConfig struct {
	ReadonlyRootfs bool
	Binds          []string
	CapDrop        []string
	SecurityOpt    []string
	PidsLimit      int64
	Memory         int64
	MemorySwap     int64
	NetworkMode    string
	RestartPolicy  restartPolicy
}

const egressMemory = 32 << 20

// egressSpec is the egress proxy's fixed spec: the clawbridge-agentd image
// by digest running "egress", as nobody, read-only, with no capabilities,
// on its outbound network. Its one host path is the policy directory,
// read-only; it has no env and no secrets. agentd connects it to each
// owner's network, where the agents reach it as clawbridge-egress:3128.
func egressSpec(image, policyDir string) egressCreate {
	return egressCreate{
		Image: image,
		User:  "65534:65534",
		Cmd:   []string{"egress", "--listen", "0.0.0.0:" + egressPort, "--policy", policyMount + "/" + policyFile},
		HostConfig: egressHostConfig{
			ReadonlyRootfs: true,
			Binds:          []string{policyDir + ":" + policyMount + ":ro"},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			PidsLimit:      64,
			Memory:         egressMemory,
			MemorySwap:     egressMemory,
			NetworkMode:    egressNetName,
			RestartPolicy:  restartPolicy{Name: "no"},
		},
	}
}

// connectSpec is the body of POST /networks/{id}/connect and disconnect:
// only the egress proxy ever joins an owner's network.
type connectSpec struct{ Container string }

// validImage reports an error unless image is pinned by digest.
func validImage(image string) error {
	if !imagePattern.MatchString(image) {
		return fmt.Errorf("the agent image must be pinned by digest (name@sha256:…), not %q", image)
	}
	return nil
}
