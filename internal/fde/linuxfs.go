package fde

import (
	"context"
	"errors"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

const (
	methodLUKS    = "dm-crypt"
	methodZFS     = "zfs"
	methodEcrypt  = "ecryptfs"
	methodFscrypt = "fscrypt"
	methodMount   = "mount"
)

var lookPath = exec.LookPath

var errUntrustedTool = errors.New("tool not found in a system directory")

// runLinuxTool runs a tool found through the PATH, but only when it resolves
// into /usr/bin, /usr/sbin, /bin or /sbin. Their locations vary by distro, so
// no fixed path works; a tool found anywhere else counts as unknown.
func runLinuxTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	p, err := lookPath(name)
	if err != nil {
		return nil, err
	}
	switch path.Dir(p) {
	case "/usr/bin", "/usr/sbin", "/bin", "/sbin":
		return runCmd(ctx, p, args...)
	}
	return nil, errUntrustedTool
}

// blockTypes are lsblk TYPE values of a local block-device chain. A chain
// that contains any other type (loop, rom, an empty answer) cannot be shown
// to be unencrypted.
func localBlockType(t string) bool {
	switch {
	case t == "disk", t == "part", t == "lvm", t == "dm", t == "mpath":
		return true
	case strings.HasPrefix(t, "raid"):
		return true
	}
	return false
}

var zfsValue = regexp.MustCompile(`^[a-z0-9-]+$`)

// detectLinux checks the filesystem that holds dir. fscrypt is consulted
// whenever the filesystem check did not already find encryption.
func detectLinux(ctx context.Context, dir string) Result {
	r := detectLinuxFS(ctx, dir)
	if r.State == StateDetected {
		return r
	}
	if out, err := runLinuxTool(ctx, "fscrypt", "status", dir); err == nil && fscryptEncrypted(out, dir) {
		return result(StateDetected, methodFscrypt, "directory is encrypted with fscrypt")
	}
	return r
}

func detectLinuxFS(ctx context.Context, dir string) Result {
	out, err := runLinuxTool(ctx, "findmnt", "-n", "-o", "SOURCE,FSTYPE", "--target", dir)
	if err != nil {
		return unknown(methodMount, "couldn't find the filesystem")
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return unknown(methodMount, "couldn't find the filesystem")
	}
	source, fstype := f[0], f[1]
	switch {
	case fstype == "zfs":
		return detectZFS(ctx, source)
	case fstype == "ecryptfs":
		return result(StateDetected, methodEcrypt, "ecryptfs mount")
	case fstype == "tmpfs", fstype == "overlay", strings.HasPrefix(fstype, "nfs"),
		fstype == "cifs", strings.HasPrefix(fstype, "smb"), strings.HasPrefix(fstype, "fuse"):
		return unknown(methodMount, fstype+" filesystem")
	}
	// btrfs reports SOURCE as /dev/sda2[/@subvolume].
	if i := strings.IndexByte(source, '['); i >= 0 {
		source = source[:i]
	}
	if !strings.HasPrefix(source, "/dev/") {
		return unknown(methodMount, "filesystem isn't on a local block device")
	}
	return detectBlock(ctx, source)
}

func detectZFS(ctx context.Context, dataset string) Result {
	// Pool and dataset names can be a hostname or a person's name, so Detail
	// never carries them.
	out, err := runLinuxTool(ctx, "zfs", "get", "-H", "-o", "value", "encryption", dataset)
	if err != nil {
		return unknown(methodZFS, "zfs failed for the dataset")
	}
	v := strings.TrimSpace(string(out))
	switch {
	case v == "off":
		return result(StateNotDetected, methodZFS, "zfs dataset not encrypted")
	case v == "-" || !zfsValue.MatchString(v):
		return unknown(methodZFS, "zfs doesn't report encryption for the dataset")
	}
	return result(StateDetected, methodZFS, "zfs dataset encrypted")
}

func detectBlock(ctx context.Context, source string) Result {
	dev := path.Base(source)
	out, err := runLinuxTool(ctx, "lsblk", "-s", "-n", "-o", "TYPE,NAME", source)
	if err != nil {
		return unknown(methodLUKS, "lsblk failed for "+dev)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	allLocal := true
	seen := 0
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		seen++
		if f[0] == "crypt" {
			return result(StateDetected, methodLUKS, "dm-crypt layer under "+dev)
		}
		if !localBlockType(f[0]) {
			allLocal = false
		}
	}
	if seen == 0 || !allLocal {
		return unknown(methodLUKS, "couldn't map the device tree of "+dev)
	}
	return result(StateNotDetected, methodLUKS, "no dm-crypt layer under "+dev)
}

func detectLinuxOrWSL(ctx context.Context, dir string) Result {
	if isWSL() {
		return windowsPolicy(ctx)
	}
	return detectLinux(ctx, dir)
}

// fscryptEncrypted reports whether a whole line of the output equals
// `"<dir>" is encrypted with fscrypt.`, trailing whitespace aside. The tool
// echoes the path, so a substring match could be fooled by a directory name.
func fscryptEncrypted(out []byte, dir string) bool {
	line := `"` + dir + `" is encrypted with fscrypt.`
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimRight(l, " \t\r") == line {
			return true
		}
	}
	return false
}
