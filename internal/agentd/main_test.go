package agentd

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	env := map[string]string{"CLAWBRIDGE_SERVER_URL": testServer, "CLAWBRIDGE_AGENT_IMAGE": testImage}
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
	if got := proxied.send(t, http.MethodGet, "/info", nil); got != http.StatusForbidden {
		t.Fatalf("info: %d", got)
	}
	if got := proxied.send(t, http.MethodPost, "/containers/create?name=clawbridge-agent-"+refA, mustJSON(t, agentSpec(testImage, testServer, refA))); got != http.StatusCreated {
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
		"CLAWBRIDGE_SERVER_URL":     testServer,
		"CLAWBRIDGE_AGENT_IMAGE":    testImage,
		"CLAWBRIDGE_LAUNCHER_TOKEN": "cbl_" + strings.Repeat("x", 24),
		"CLAWBRIDGE_AGENTD_DOCKER":  "/dev/null",
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
	var out bytes.Buffer
	if err := Main(context.Background(), []string{"check"}, func(k string) string { return good[k] }, &out, io.Discard); err != nil || !strings.Contains(out.String(), "ok") {
		t.Fatal(err, out.String())
	}
	if err := run(good, "run"); !errors.Is(err, errNoLauncherEndpoints) {
		t.Fatal(err)
	}
}
