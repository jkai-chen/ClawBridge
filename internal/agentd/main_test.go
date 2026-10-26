package agentd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("agentd refuses to run as root")
	}
}

func TestMainProxyServesAndRefuses(t *testing.T) {
	skipAsRoot(t)
	s := newStack(t)
	dir := socketDir(t)
	upstream := filepath.Join(dir, "docker.sock")
	serveUnix(t, upstream, s.daemon)
	listen := filepath.Join(dir, "proxy.sock")
	env := map[string]string{"CLAWBRIDGE_SERVER_URL": testServer, "CLAWBRIDGE_AGENT_IMAGE": testImage,
		"CLAWBRIDGE_EGRESS_IMAGE": testEgressImage, "CLAWBRIDGE_EGRESS_POLICY_DIR": "/var/lib/clawbridge-agentd/egress"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Main(ctx, []string{"proxy", "--listen", listen, "--upstream", upstream}, func(k string) string { return env[k] }, io.Discard, io.Discard)
	}()
	proxied := &testStack{daemon: s.daemon, proxy: listen}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if checkSocket(listen) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the proxy did not listen")
		}
	}
	if info, err := os.Stat(listen); err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", info.Mode(), err)
	}
	if got := proxied.send(t, http.MethodGet, "/info", nil); got != http.StatusForbidden {
		t.Fatalf("info: %d", got)
	}
	if got := proxied.send(t, http.MethodPost, "/containers/create?name=clawbridge-agent-"+refA, mustJSON(t, agentSpec(testImage, testServer, refA, 1))); got != http.StatusCreated {
		t.Fatalf("create: %d", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMainRefuses(t *testing.T) {
	skipAsRoot(t)
	dir := socketDir(t)
	good := map[string]string{
		"CLAWBRIDGE_SERVER_URL":        testServer,
		"CLAWBRIDGE_AGENT_IMAGE":       testImage,
		"CLAWBRIDGE_EGRESS_IMAGE":      testEgressImage,
		"CLAWBRIDGE_EGRESS_POLICY_DIR": "/var/lib/clawbridge-agentd/egress",
		"CLAWBRIDGE_LAUNCHER_TOKEN":    "cbl_" + strings.Repeat("x", 24),
		"CLAWBRIDGE_AGENTD_DOCKER":     "/dev/null",
	}
	run := func(env map[string]string, args ...string) error {
		return Main(context.Background(), args, func(k string) string { return env[k] }, io.Discard, io.Discard)
	}
	for name, c := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"no command":       {good, nil, "usage"},
		"unknown command":  {good, []string{"shell"}, "usage"},
		"proxy with token": {good, []string{"proxy", "--listen", dir + "/p.sock", "--upstream", dir + "/d.sock"}, "holds no secrets"},
		"proxy on root":    {map[string]string{}, []string{"proxy", "--listen", dir + "/p.sock", "--upstream", "/var/run/docker.sock"}, "root Docker daemon"},
		"proxy loop":       {map[string]string{}, []string{"proxy", "--listen", dir + "/p.sock", "--upstream", dir + "/p.sock"}, "its upstream"},
		"proxy root owned": {map[string]string{}, []string{"proxy", "--listen", dir + "/p.sock", "--upstream", "/dev/null"}, "owned by root"},
		"check root owned": {good, []string{"check"}, "owned by root"},
	} {
		if err := run(c.env, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// With a valid configuration, check passes and run says what it waits for.
	sock := filepath.Join(dir, "agentd.sock")
	serveUnix(t, sock, http.NotFoundHandler())
	good["CLAWBRIDGE_AGENTD_DOCKER"] = sock
	if err := run(good, "check"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatal(err)
	}
	good["CLAWBRIDGE_EGRESS_POLICY_DIR"] = t.TempDir()
	var out bytes.Buffer
	if err := Main(context.Background(), []string{"check"}, func(k string) string { return good[k] }, &out, io.Discard); err != nil || !strings.Contains(out.String(), "ok") {
		t.Fatal(err, out.String())
	}
	if err := run(good, "run"); !errors.Is(err, errNoLauncherEndpoints) {
		t.Fatal(err)
	}
}

// The egress command serves the policy file, follows its changes and fails
// closed on an invalid one. Only refusals are exercised: it never dials out.
func TestMainEgressReloadsAndFailsClosed(t *testing.T) {
	skipAsRoot(t)
	old := egressPoll
	egressPoll = 20 * time.Millisecond
	t.Cleanup(func() { egressPoll = old })
	dir := t.TempDir()
	policy := filepath.Join(dir, "egress.json")
	write := func(s string) {
		if err := os.WriteFile(policy, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	none := `{"networks":[{"owner_ref":"p_aaaaaaaa","source":"172.30.1.0/24","allow":["bridge.example.test:443"]}]}`
	mine := `{"networks":[{"owner_ref":"p_aaaaaaaa","source":"127.0.0.0/8","allow":["bridge.example.test:443"]}]}`
	run := func(args ...string) error {
		return Main(context.Background(), args, func(string) string { return "" }, io.Discard, io.Discard)
	}
	write(`{"networks":[],"allow_all":true}`)
	if err := run("egress", "--listen", "127.0.0.1:0", "--policy", policy); err == nil || !strings.Contains(err.Error(), "allow_all") {
		t.Fatal(err)
	}
	if err := Main(context.Background(), []string{"egress", "--listen", "127.0.0.1:0", "--policy", policy},
		func(k string) string { return map[string]string{"CLAWBRIDGE_LAUNCHER_TOKEN": "cbl_x"}[k] }, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no secrets") {
		t.Fatal(err)
	}
	if err := run("egress", "--listen", "localhost:3128", "--policy", policy); err == nil {
		t.Fatal("a host name listen address was accepted")
	}

	// Find a free port, then serve on it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	write(none)
	logs := &lockedWriter{w: &bytes.Buffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Main(ctx, []string{"egress", "--listen", addr, "--policy", policy}, func(string) string { return "" }, io.Discard, logs)
	}()
	// connect sends a CONNECT for evil.test and returns the logged reason.
	connect := func() string {
		t.Helper()
		var conn net.Conn
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if conn, err = net.Dial("tcp", addr); err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		before := strings.Count(logs.String(), "egress refused")
		io.WriteString(conn, "CONNECT evil.test:443 HTTP/1.1\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%v %v", resp, err)
		}
		for deadline := time.Now().Add(5 * time.Second); strings.Count(logs.String(), "egress refused") == before && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		return lines[len(lines)-1]
	}
	waitLog := func(s string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), s); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q in %s", s, logs.String())
			}
		}
	}
	if got := connect(); !strings.Contains(got, "no owner's network") {
		t.Fatal(got)
	}
	write(mine)
	waitLog("egress policy reloaded")
	if got := connect(); !strings.Contains(got, "allow-list") {
		t.Fatal(got)
	}
	write("{not json")
	waitLog("refusing all egress")
	if got := connect(); !strings.Contains(got, "no owner's network") {
		t.Fatal(got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
