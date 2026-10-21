package auth

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAvailabilityNeverLaunchesBusOrEchoesAddress(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	if err := os.WriteFile(filepath.Join(dir, "dbus-launch"), []byte("#!/bin/sh\n : > '"+marker+"'\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	for _, address := range []string{"", "autolaunch:", "unix:path=/nonexistent/synthetic-private-address"} {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
		err := checkLinuxAvailability(context.Background())
		if err == nil || !strings.Contains(err.Error(), "same user D-Bus session") || strings.Contains(err.Error(), "synthetic-private-address") {
			t.Fatalf("unsafe guidance: %v", err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("dbus-launch invoked")
	}
}

func TestAvailabilityRestrictsDialToConcreteLocalSockets(t *testing.T) {
	for _, a := range []string{"tcp:host=synthetic-private-host,port=1234", "autolaunch:", "unix:path=/a;unix:path=/b", "unix:path=/a,path=/b", "unix:path=/a,abstract=b", "unix:path=relative"} {
		if _, err := localBusSocket(a); err == nil {
			t.Fatalf("accepted unsupported bus %q", a)
		}
	}
	for a, want := range map[string]string{"unix:path=/tmp/bus%20socket,guid=abc": "/tmp/bus socket", "unix:abstract=session": "@session"} {
		got, err := localBusSocket(a)
		if err != nil || got != want {
			t.Fatalf("local socket: %q %v", got, err)
		}
	}
}

func TestAvailabilityBoundsAuthenticationOnExistingSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix session-bus transport")
	}
	dir, err := os.MkdirTemp("", "cb-bus-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "bus")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	defer close(release)
	go func() {
		c, e := listener.Accept()
		if e == nil {
			defer c.Close()
			<-release
		}
	}()
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+socket)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err = checkLinuxAvailability(ctx); err == nil {
		t.Fatal("unresponsive bus accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("authentication exceeded context bound")
	}
}
