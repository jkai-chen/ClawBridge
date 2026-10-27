package agentd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	token := "cbl_" + strings.Repeat("s3cr3t", 5)
	env := map[string]string{
		"CLAWBRIDGE_SERVER_URL":        testServer,
		"CLAWBRIDGE_AGENT_IMAGE":       testImage,
		"CLAWBRIDGE_EGRESS_IMAGE":      testEgressImage,
		"CLAWBRIDGE_EGRESS_POLICY_DIR": "/var/lib/clawbridge-agentd/egress",
		"CLAWBRIDGE_LAUNCHER_TOKEN":    token,
		"CLAWBRIDGE_AGENTD_DOCKER":     "/run/clawbridge-agentd/docker.sock",
	}
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil || c.LauncherToken != token || c.Docker != env["CLAWBRIDGE_AGENTD_DOCKER"] {
		t.Fatal(c, err)
	}
	for name, edit := range map[string][2]string{
		"http server":       {"CLAWBRIDGE_SERVER_URL", "http://bridge.example.test"},
		"server with user":  {"CLAWBRIDGE_SERVER_URL", "https://u:p@bridge.example.test"},
		"server with query": {"CLAWBRIDGE_SERVER_URL", "https://bridge.example.test/?x=1"},
		"no server":         {"CLAWBRIDGE_SERVER_URL", ""},
		"tag only":          {"CLAWBRIDGE_AGENT_IMAGE", "ghcr.io/example/agent:latest"},
		"short digest":      {"CLAWBRIDGE_AGENT_IMAGE", "ghcr.io/example/agent@sha256:0123"},
		"no image":          {"CLAWBRIDGE_AGENT_IMAGE", ""},
		"egress tag only":   {"CLAWBRIDGE_EGRESS_IMAGE", "ghcr.io/example/agentd:latest"},
		"no egress image":   {"CLAWBRIDGE_EGRESS_IMAGE", ""},
		"relative policy":   {"CLAWBRIDGE_EGRESS_POLICY_DIR", "egress"},
		"policy with colon": {"CLAWBRIDGE_EGRESS_POLICY_DIR", "/var/lib/x:/etc"},
		"no policy dir":     {"CLAWBRIDGE_EGRESS_POLICY_DIR", ""},
		"localhost server":  {"CLAWBRIDGE_SERVER_URL", "http://localhost:8080"},
		"owner token":       {"CLAWBRIDGE_LAUNCHER_TOKEN", "cbo_" + strings.Repeat("s3cr3t", 5)},
		"short token":       {"CLAWBRIDGE_LAUNCHER_TOKEN", "cbl_s3cr3t"},
		"root daemon":       {"CLAWBRIDGE_AGENTD_DOCKER", "/var/run/docker.sock"},
		"root daemon, run":  {"CLAWBRIDGE_AGENTD_DOCKER", "/run/docker.sock"},
		"relative socket":   {"CLAWBRIDGE_AGENTD_DOCKER", "docker.sock"},
		"unclean socket":    {"CLAWBRIDGE_AGENTD_DOCKER", "/run/../var/run/docker.sock"},
		"private server":    {"CLAWBRIDGE_SERVER_URL", "https://10.0.0.5"},
		"loopback server":   {"CLAWBRIDGE_SERVER_URL", "https://127.0.0.1:8443"},
		"cgnat server":      {"CLAWBRIDGE_SERVER_URL", "https://100.64.1.1"},
		"metadata server":   {"CLAWBRIDGE_SERVER_URL", "https://[fe80::1]"},
	} {
		bad := map[string]string{}
		for k, v := range env {
			bad[k] = v
		}
		bad[edit[0]] = edit[1]
		_, err := LoadConfig(func(k string) string { return bad[k] })
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("%s: the token was echoed: %v", name, err)
		}
	}
	if err := validServerURL("http://localhost:8080"); err != nil {
		t.Fatal(err)
	}
	// A public IP literal is a server the agents can reach [R90-L5].
	env["CLAWBRIDGE_SERVER_URL"] = "https://203.0.113.5:8443"
	if _, err := LoadConfig(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSocket(t *testing.T) {
	dir := socketDir(t)
	path := filepath.Join(dir, "ok.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if os.Geteuid() != 0 {
		if err := checkSocket(path); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{file, filepath.Join(dir, "missing"), "/dev/null"} {
		if checkSocket(bad) == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if err := checkSocket("/dev/null"); err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatal(err)
	}
	geteuid = func() int { return 0 }
	defer func() { geteuid = os.Geteuid }()
	if notRoot() == nil {
		t.Fatal("root accepted")
	}
}

// CLAWBRIDGE_EGRESS_PRIVATE_SERVER lets a private server through, when it
// names the server's own host:port (Q2).
func TestLoadConfigPrivateServer(t *testing.T) {
	base := map[string]string{
		"CLAWBRIDGE_AGENT_IMAGE":       testImage,
		"CLAWBRIDGE_EGRESS_IMAGE":      testEgressImage,
		"CLAWBRIDGE_EGRESS_POLICY_DIR": "/var/lib/clawbridge-agentd/egress",
		"CLAWBRIDGE_LAUNCHER_TOKEN":    "cbl_" + strings.Repeat("x", 24),
		"CLAWBRIDGE_AGENTD_DOCKER":     "/run/clawbridge-agentd/docker.sock",
	}
	load := func(server, private string) (Config, error) {
		env := map[string]string{"CLAWBRIDGE_SERVER_URL": server, "CLAWBRIDGE_EGRESS_PRIVATE_SERVER": private}
		return LoadConfig(func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return base[k]
		})
	}
	for _, c := range []struct{ server, private, want string }{
		{"https://10.0.0.5", "10.0.0.5:443", "10.0.0.5:443"},
		{"https://bridge.lan.test:8443", "Bridge.LAN.test:8443", "bridge.lan.test:8443"},
		{"https://100.100.1.2", "100.100.1.2:443", "100.100.1.2:443"},
		{"https://bridge.example.test", "", ""},
	} {
		cfg, err := load(c.server, c.private)
		if err != nil || cfg.PrivateServer != c.want {
			t.Fatalf("%s %s: %q %v", c.server, c.private, cfg.PrivateServer, err)
		}
	}
	if _, err := load("https://203.0.113.5", "203.0.113.5:443"); err == nil || !strings.Contains(err.Error(), "leave it unset") {
		t.Fatalf("a listed public server: %v", err)
	}
	for _, c := range [][2]string{
		{"https://10.0.0.5", ""},
		{"https://10.0.0.5", "10.0.0.6:443"},
		{"https://10.0.0.5", "10.0.0.5:8443"},
		{"https://203.0.113.5", "203.0.113.5:443"},
		{"https://127.0.0.1", "127.0.0.1:443"},
		{"https://169.254.169.254", "169.254.169.254:443"},
		{"https://172.30.0.5", "172.30.0.5:443"},
		{"https://bridge.lan.test", "other.lan.test:443"},
	} {
		if _, err := load(c[0], c[1]); err == nil {
			t.Fatalf("%s with %q accepted", c[0], c[1])
		}
	}
}
