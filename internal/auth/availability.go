package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const LinuxKeyringGuidance = "Linux Secret Service is unavailable: ClawBridge and the unlocked keyring must share the same user D-Bus session. Restore that session and see README.md (Linux credential storage). Device login does not bypass the keyring; no plaintext fallback is available."

// CheckNativeAvailability checks Linux bus/service presence without reading or
// writing credentials, activating services, or claiming unlock/write readiness.
// Other platforms rely on their native credential operation's failure boundary.
func CheckNativeAvailability(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return ctx.Err()
	}
	return checkLinuxAvailability(ctx)
}

func checkLinuxAvailability(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" {
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
		}
		if !filepath.IsAbs(runtimeDir) {
			return errors.New(LinuxKeyringGuidance)
		}
		socket := filepath.Join(runtimeDir, "bus")
		if info, e := os.Stat(socket); e != nil || info.Mode()&os.ModeSocket == 0 {
			return errors.New(LinuxKeyringGuidance)
		}
		address = "unix:path=" + dbus.EscapeBusAddressValue(socket)
	}
	// Availability uses only a concrete local Unix session bus; no autolaunch,
	// TCP transport, address fallback, or unbounded library dial is permitted.
	socket, err := localBusSocket(address)
	if err != nil {
		return errors.New(LinuxKeyringGuidance)
	}
	transport, err := (&net.Dialer{}).DialContext(bounded, "unix", socket)
	if err != nil {
		return errors.New(LinuxKeyringGuidance)
	}
	conn, err := dbus.NewConn(transport, dbus.WithContext(bounded))
	if err != nil {
		transport.Close()
		return errors.New(LinuxKeyringGuidance)
	}
	defer conn.Close()
	if conn.Auth(nil) != nil || conn.Hello() != nil {
		return errors.New(LinuxKeyringGuidance)
	}
	var present bool
	err = conn.BusObject().CallWithContext(bounded, "org.freedesktop.DBus.NameHasOwner", 0, "org.freedesktop.secrets").Store(&present)
	if err != nil || !present {
		return errors.New(LinuxKeyringGuidance)
	}
	return nil
}

func nativeStoreError(operation string) error {
	if runtime.GOOS == "linux" {
		return errors.New("cannot " + operation + " the ClawBridge credential in the OS keychain. " + LinuxKeyringGuidance)
	}
	return errors.New("cannot " + operation + " the ClawBridge credential in the OS keychain; ensure the native credential store is available and unlocked")
}

func localBusSocket(address string) (string, error) {
	invalid := errors.New("existing local Unix session bus required")
	if !strings.HasPrefix(address, "unix:") || strings.Contains(address, ";") {
		return "", invalid
	}
	values := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(address, "unix:"), ",") {
		key, raw, ok := strings.Cut(part, "=")
		if !ok {
			return "", invalid
		}
		if _, exists := values[key]; exists {
			return "", invalid
		}
		value, err := dbus.UnescapeBusAddressValue(raw)
		if err != nil {
			return "", invalid
		}
		values[key] = value
	}
	path, abstract := values["path"], values["abstract"]
	if path != "" && abstract == "" && filepath.IsAbs(path) {
		return path, nil
	}
	if abstract != "" && path == "" {
		return "@" + abstract, nil
	}
	return "", invalid
}
