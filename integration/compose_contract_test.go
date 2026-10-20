package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type renderedCompose struct {
	Services map[string]renderedService `json:"services"`
	Networks map[string]renderedNetwork `json:"networks"`
	Volumes  map[string]renderedVolume  `json:"volumes"`
}

type renderedService struct {
	Command     []string              `json:"command"`
	DependsOn   map[string]any        `json:"depends_on"`
	Environment map[string]string     `json:"environment"`
	Image       string                `json:"image"`
	Networks    map[string]any        `json:"networks"`
	Ports       []renderedPort        `json:"ports"`
	Volumes     []renderedVolumeMount `json:"volumes"`
}

type renderedPort struct {
	Mode      string `json:"mode"`
	Target    int    `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
}

type renderedVolumeMount struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

type renderedNetwork struct {
	Internal bool `json:"internal"`
}

type renderedVolume struct {
	Name string `json:"name"`
}

func TestComposePublicHTTPSAndPersistenceContract(t *testing.T) {
	const (
		browserClientSecret = "synthetic-compose-browser-app-credential"
		deviceClientSecret  = "synthetic-compose-device-app-credential"
	)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	root := repositoryRoot(t)
	composePath := filepath.Join(root, "deploy", "compose.yaml")
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"POSTGRES_PASSWORD=review-only-database-password",
		"CLAWBRIDGE_AUDIENCE=browser-client.apps.googleusercontent.com,device-client.apps.googleusercontent.com",
		"CLAWBRIDGE_BOOTSTRAP_OWNER=owner@example.test",
		"CLAWBRIDGE_OAUTH_CLIENT_ID=browser-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID=device-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_CLIENT_SECRET="+browserClientSecret,
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET="+deviceClientSecret,
		"CLAWBRIDGE_DOMAIN=clawbridge.example.test",
		"CLAWBRIDGE_POSTGRES_DATA_PATH=/srv/clawbridge/postgres-data",
	)
	cmd := exec.Command("docker", "compose", "-f", composePath, "config", "--format", "json")
	cmd.Dir = root
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render compose contract fixture: %v\n%s", err, output)
	}
	compose, err := decodeRenderedCompose(output)
	if err != nil {
		t.Fatalf("decode rendered Compose: %v\n%s", err, output)
	}

	backend, ok := compose.Networks["backend"]
	if !ok || !backend.Internal {
		t.Fatalf("backend network must be internal: %#v", compose.Networks)
	}
	egress, ok := compose.Networks["egress"]
	if !ok || egress.Internal {
		t.Fatalf("egress network must be present and non-internal: %#v", compose.Networks)
	}
	assertNetworks(t, compose.Services["db"].Networks, "db", "backend")
	assertNetworks(t, compose.Services["api"].Networks, "api", "backend", "egress")
	assertNetworks(t, compose.Services["caddy"].Networks, "caddy", "backend", "egress")
	if got := compose.Services["api"].Environment["CLAWBRIDGE_OAUTH_CLIENT_SECRET"]; got != browserClientSecret {
		t.Fatal("API browser OAuth app credential was not sourced from its matching environment variable")
	}
	if got := compose.Services["api"].Environment["CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET"]; got != deviceClientSecret {
		t.Fatal("API device OAuth app credential was not sourced from its matching environment variable")
	}
	for serviceName, service := range compose.Services {
		joinedCommand := strings.Join(service.Command, "\x00")
		if strings.Contains(joinedCommand, browserClientSecret) || strings.Contains(joinedCommand, deviceClientSecret) {
			t.Fatalf("%s command or argv contains an installed-app OAuth credential", serviceName)
		}
		encodedEnvironment, _ := json.Marshal(service.Environment)
		if serviceName != "api" &&
			(strings.Contains(string(encodedEnvironment), browserClientSecret) ||
				strings.Contains(string(encodedEnvironment), deviceClientSecret)) {
			t.Fatalf("%s environment contains an unrelated installed-app OAuth credential", serviceName)
		}
	}

	ports := compose.Services["caddy"].Ports
	if len(ports) != 1 || ports[0].Target != 443 || ports[0].Published != "443" ||
		ports[0].Protocol != "tcp" || ports[0].Mode != "ingress" {
		t.Fatalf("Caddy must publish exactly TCP 443: %#v", ports)
	}
	if ports := compose.Services["api"].Ports; len(ports) != 0 {
		t.Fatalf("API must not publish a host port: %#v", ports)
	}
	if ports := compose.Services["db"].Ports; len(ports) != 0 {
		t.Fatalf("database must not publish a host port: %#v", ports)
	}

	dbVolume := findVolumeMount(compose.Services["db"].Volumes, "/var/lib/postgresql/data")
	if dbVolume == nil {
		t.Fatalf("database data mount missing: %#v", compose.Services["db"].Volumes)
	}
	if dbVolume.Type != "bind" || dbVolume.Source != "/srv/clawbridge/postgres-data" {
		t.Fatalf("database data path did not render as the configured host bind: %#v", *dbVolume)
	}
	if _, ok := compose.Volumes["postgres-data"]; ok {
		t.Fatalf("absolute host data path must not also declare a named PostgreSQL volume: %#v", compose.Volumes)
	}

	for _, volumeName := range []string{"caddy-data", "caddy-config"} {
		if _, ok := compose.Volumes[volumeName]; !ok {
			t.Fatalf("Caddy persistence volume %q missing: %#v", volumeName, compose.Volumes)
		}
	}
	if findVolumeMount(compose.Services["caddy"].Volumes, "/data") == nil {
		t.Fatal("Caddy certificate/data volume missing")
	}
	if findVolumeMount(compose.Services["caddy"].Volumes, "/config") == nil {
		t.Fatal("Caddy configuration volume missing")
	}
	if strings.Contains(string(output), "80:80") {
		t.Fatal("Compose requires public port 80; owner requires inbound 443 only")
	}
	if !containsAll(compose.Services["caddy"].Command, "reverse-proxy", "--disable-redirects") {
		t.Fatalf("Caddy must disable HTTP redirects for inbound-443-only operation: %#v", compose.Services["caddy"].Command)
	}

	defaultEnv := append([]string{}, os.Environ()...)
	defaultEnv = append(defaultEnv,
		"POSTGRES_PASSWORD=review-only-database-password",
		"CLAWBRIDGE_AUDIENCE=browser-client.apps.googleusercontent.com,device-client.apps.googleusercontent.com",
		"CLAWBRIDGE_BOOTSTRAP_OWNER=owner@example.test",
		"CLAWBRIDGE_OAUTH_CLIENT_ID=browser-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID=device-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_CLIENT_SECRET="+browserClientSecret,
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET="+deviceClientSecret,
		"CLAWBRIDGE_DOMAIN=clawbridge.example.test",
	)
	cmd = exec.Command("docker", "compose", "-f", composePath, "config", "--format", "json")
	cmd.Dir = root
	cmd.Env = withoutEnv(defaultEnv, "CLAWBRIDGE_POSTGRES_DATA_PATH")
	output, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render default Compose persistence: %v\n%s", err, output)
	}
	compose, err = decodeRenderedCompose(output)
	if err != nil {
		t.Fatalf("decode default rendered Compose: %v\n%s", err, output)
	}
	dbVolume = findVolumeMount(compose.Services["db"].Volumes, "/var/lib/postgresql/data")
	if dbVolume == nil || dbVolume.Type != "volume" || dbVolume.Source != "postgres-data" {
		t.Fatalf("default database persistence must remain the named volume: %#v", dbVolume)
	}
}

func decodeRenderedCompose(raw []byte) (renderedCompose, error) {
	var compose renderedCompose
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&compose); err != nil {
		return renderedCompose{}, err
	}
	return compose, nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}

func assertNetworks(t *testing.T, got map[string]any, service string, want ...string) {
	t.Helper()
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("%s networks=%v, want %v", service, names, want)
	}
}

func findVolumeMount(mounts []renderedVolumeMount, target string) *renderedVolumeMount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

func withoutEnv(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func containsAll(values []string, want ...string) bool {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range want {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}
