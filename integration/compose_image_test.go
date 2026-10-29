package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The published-image override pulls exactly CLAWBRIDGE_IMAGE and can't
// build; everything else about api is the source path's. Without the
// variable it refuses to render.
func TestComposeImageOverride(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	deploy := filepath.Join(repositoryRoot(t), "deploy")
	image := "ghcr.io/jkai-chen/clawbridge@sha256:" + strings.Repeat("ab", 32)
	env := append(os.Environ(),
		"POSTGRES_PASSWORD=review-only-database-password",
		"CLAWBRIDGE_BOOTSTRAP_OWNER=owner@example.test",
		"CLAWBRIDGE_DOMAIN=clawbridge.example.test",
	)
	render := func(extra []string, files ...string) (map[string]any, string) {
		t.Helper()
		args := []string{"compose", "--project-name", "clawbridge-image-contract"}
		for _, f := range files {
			args = append(args, "-f", filepath.Join(deploy, f))
		}
		cmd := exec.Command("docker", append(args, "config", "--format", "json")...)
		cmd.Env = append(append([]string{}, env...), extra...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, string(out)
		}
		var doc struct {
			Services map[string]map[string]any `json:"services"`
		}
		if err = json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Services["api"], ""
	}
	source, failure := render(nil, "compose.yaml")
	if source == nil {
		t.Fatal(failure)
	}
	if _, ok := source["build"]; !ok {
		t.Fatal("the source path lost its build")
	}
	published, failure := render([]string{"CLAWBRIDGE_IMAGE=" + image}, "compose.yaml", "compose.image.yaml")
	if published == nil {
		t.Fatal(failure)
	}
	if _, ok := published["build"]; ok || published["image"] != image || published["pull_policy"] != "missing" {
		t.Fatalf("published api: build=%v image=%v pull_policy=%v", published["build"], published["image"], published["pull_policy"])
	}
	for _, key := range []string{"command", "environment", "depends_on", "networks", "volumes", "user", "read_only"} {
		if !reflect.DeepEqual(source[key], published[key]) {
			t.Errorf("%s differs between the paths: %v vs %v", key, source[key], published[key])
		}
	}
	if api, failure := render([]string{"CLAWBRIDGE_IMAGE="}, "compose.yaml", "compose.image.yaml"); api != nil || !strings.Contains(failure, "CLAWBRIDGE_IMAGE") {
		t.Fatal("rendered without an image:", failure)
	}
}
