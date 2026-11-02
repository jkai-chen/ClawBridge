package fde

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"path"
	"regexp"
	"strings"
)

const methodMacOS = "filevault"

// Absolute tool paths, so a hostile PATH cannot stand in for them.
const (
	dfTool       = "/bin/df"
	fdesetupTool = "/usr/bin/fdesetup"
	diskutilTool = "/usr/sbin/diskutil"
)

var capacityField = regexp.MustCompile(`^\d+%$`)

// dfMount returns the mount point from `df -P <dir>` output.
func dfMount(out []byte) (string, bool) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return "", false
	}
	f := strings.Fields(lines[1])
	for i := 1; i < len(f)-1; i++ {
		if capacityField.MatchString(f[i]) {
			return strings.Join(f[i+1:], " "), true
		}
	}
	return "", false
}

// safeMount keeps a mount point only when it cannot carry a username.
func safeMount(m string) string {
	if m == "/" || m == "/System/Volumes/Data" || strings.HasPrefix(m, "/Volumes/") {
		return m
	}
	return path.Base(m)
}

func detectDarwin(ctx context.Context, dir string) Result {
	out, err := runCmd(ctx, dfTool, "-P", dir)
	if err != nil {
		return unknown(methodMacOS, "couldn't find the volume")
	}
	mount, ok := dfMount(out)
	if !ok || !strings.HasPrefix(mount, "/") {
		return unknown(methodMacOS, "couldn't find the volume")
	}
	shown := safeMount(mount)
	if mount == "/" || mount == "/System/Volumes/Data" {
		out, err := runCmd(ctx, fdesetupTool, "status")
		if err != nil {
			return unknown(methodMacOS, "fdesetup failed for "+shown)
		}
		// An exact whole-line match, never a prefix match. While FileVault is
		// encrypting or decrypting, fdesetup prints a second line after
		// "FileVault is On."; that must stay unknown.
		switch strings.TrimSpace(string(out)) {
		case "FileVault is On.":
			return result(StateDetected, methodMacOS, "FileVault is on for "+shown)
		case "FileVault is Off.":
			return result(StateNotDetected, methodMacOS, "FileVault is off for "+shown)
		}
		return unknown(methodMacOS, "unrecognised fdesetup output for "+shown)
	}
	out, err = runCmd(ctx, diskutilTool, "info", "-plist", mount)
	if err != nil {
		return unknown("diskutil", "diskutil failed for "+shown)
	}
	keys, ok := plistBools(out, "FileVault", "Encrypted")
	if !ok {
		return unknown("diskutil", "unreadable diskutil output for "+shown)
	}
	fv, hasFV := keys["FileVault"]
	enc, hasEnc := keys["Encrypted"]
	// Internal APFS volumes on T2 and Apple-silicon Macs are hardware-encrypted
	// whether or not FileVault is on, with a key not bound to the password. So
	// Encrypted alone proves nothing: only FileVault counts as detected.
	switch {
	case hasFV && fv:
		return result(StateDetected, "diskutil", "volume "+shown+" is protected by FileVault")
	case hasEnc && enc:
		return unknown("diskutil", "volume encrypted without FileVault")
	case hasFV && hasEnc:
		return result(StateNotDetected, "diskutil", "volume "+shown+" is not encrypted")
	}
	return unknown("diskutil", "diskutil doesn't say whether "+shown+" is encrypted")
}

// plistBools reads the wanted boolean keys of the top-level dict of an XML
// plist. A key whose value is not a boolean is left out.
func plistBools(data []byte, want ...string) (map[string]bool, bool) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	got := map[string]bool{}
	depth := 0
	key := ""
	haveKey := false
	sawDict := false
	for {
		tok, err := d.Token()
		if err != nil {
			if err != io.EOF {
				return nil, false
			}
			return got, sawDict && depth == 0
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				depth++
				sawDict = true
				haveKey = false
				continue
			case "array":
				depth++
			case "key":
				if depth == 1 {
					var s string
					if d.DecodeElement(&s, &t) != nil {
						return nil, false
					}
					key, haveKey = s, true
				}
				continue
			case "true", "false":
				if depth == 1 && haveKey {
					for _, w := range want {
						if w == key {
							got[key] = t.Name.Local == "true"
						}
					}
				}
			}
			if depth == 1 {
				haveKey = false
			}
		case xml.EndElement:
			if t.Name.Local == "dict" || t.Name.Local == "array" {
				depth--
				if depth == 1 {
					haveKey = false
				}
			}
		}
	}
}
