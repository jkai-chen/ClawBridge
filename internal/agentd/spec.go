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
	"regexp"
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
// network, one env variable, and stdin open once for the agent credential.
// agentd removes exited containers itself, so Docker never restarts one.
func agentSpec(image, serverURL, ref string) createSpec {
	return createSpec{
		Image:       image,
		User:        "node",
		Env:         []string{"CLAWBRIDGE_SERVER_URL=" + serverURL},
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

// networkSpec is the body of POST /networks/create: an internal bridge, with
// no route out. The egress proxy is the only other member (§5.3).
type networkSpec struct {
	Name     string
	Driver   string
	Internal bool
}

func agentNetwork(ref string) networkSpec {
	return networkSpec{Name: networkName(ref), Driver: "bridge", Internal: true}
}

// validImage reports an error unless image is pinned by digest.
func validImage(image string) error {
	if !imagePattern.MatchString(image) {
		return fmt.Errorf("the agent image must be pinned by digest (name@sha256:…), not %q", image)
	}
	return nil
}
