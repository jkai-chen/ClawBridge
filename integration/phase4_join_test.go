package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/setup"
	"github.com/pelletier/go-toml/v2"
)

func TestPhase4JoinInputDiagnosticsExcludeAppSecrets(t *testing.T) {
	in := joinInput(filepath.Join(t.TempDir(), "trusted.toml"))
	in.OAuthClientSecret = "synthetic-browser-app-secret-P4"
	in.OAuthDeviceClientSecret = "synthetic-device-app-secret-P4"
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for name, rendered := range map[string]string{
		"json": string(raw), "plain": fmt.Sprintf("%v", in),
		"fields": fmt.Sprintf("%+v", in), "go-syntax": fmt.Sprintf("%#v", in),
	} {
		if strings.Contains(rendered, in.OAuthClientSecret) || strings.Contains(rendered, in.OAuthDeviceClientSecret) {
			t.Errorf("%s rendering exposes an installed-app secret (values suppressed)", name)
		}
	}
}

func TestPhase4JoinRequiresAuthoritativeIdentityAndScope(t *testing.T) {
	for _, name := range []string{
		"mirror", "local", "offline", "unverified", "wrong-storage", "wrong-provenance",
		"wrong-principal", "wrong-workspace", "wrong-project", "missing-workspace",
		"missing-project", "string-verified", "string-online", "empty-resolved-workspace", "empty-resolved-project",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trusted.toml")
			original := []byte("mode = 'cloud'\nunrelated = 'preserve byte-for-byte'\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			in := joinInput(path)
			e := &joinExecutor{resolveID: [2]string{"ws_selected", "prj_selected"}}
			e.result = verifiedWhoami("ws_selected", "prj_selected", in)
			data := e.result.Data.(map[string]any)
			switch name {
			case "mirror":
				e.result.Source = "offline-mirror"
			case "local":
				e.result.Source = "local-authority"
			case "offline":
				data["online"] = false
			case "unverified":
				data["verified"] = false
			case "wrong-storage":
				data["storage_mode"] = "local"
			case "wrong-provenance":
				data["identity_provenance"] = "client-supplied"
			case "wrong-principal":
				data["principal"] = "other@example.test"
			case "wrong-workspace":
				data["workspace"] = map[string]any{"id": "ws_other"}
			case "wrong-project":
				data["project"] = map[string]any{"id": "prj_other"}
			case "missing-workspace":
				delete(data, "workspace")
			case "missing-project":
				delete(data, "project")
			case "string-verified":
				data["verified"] = "true"
			case "string-online":
				data["online"] = "true"
			case "empty-resolved-workspace":
				e.resolveID[0] = ""
				data["workspace"] = map[string]any{"id": ""}
			case "empty-resolved-project":
				e.resolveID[1] = ""
				data["project"] = map[string]any{"id": ""}
			}
			if _, err := setup.Join(context.Background(), in, e); err == nil {
				t.Fatal("join accepted a response without authoritative identity and immutable scope")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatal("rejected response changed trusted configuration")
			}
			for _, req := range e.requests {
				if req.Operation != "whoami" {
					t.Fatalf("join requested unexpected operation %s", req.Operation)
				}
			}
		})
	}
}

func TestPhase4JoinPreflightsTargetBeforeAuthority(t *testing.T) {
	for _, name := range []string{"malformed", "local", "different-server", "different-principal", "symlink", "directory"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "trusted.toml")
			in := joinInput(path)
			var original []byte
			switch name {
			case "malformed":
				original = []byte("broken = [")
			case "local":
				original = []byte("mode = 'local'\n")
			case "different-server":
				original = []byte("mode = 'cloud'\nserver = 'https://other.example.test'\n")
			case "different-principal":
				original = []byte("mode = 'cloud'\nprincipal = 'another@example.test'\n")
			case "symlink":
				target := filepath.Join(dir, "target.toml")
				if err := os.WriteFile(target, []byte("mode = 'cloud'\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if original != nil {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			e := &joinExecutor{resolveID: [2]string{"ws_selected", "prj_selected"}}
			e.result = verifiedWhoami("ws_selected", "prj_selected", in)
			if _, err := setup.Join(context.Background(), in, e); err == nil {
				t.Error("join accepted an incompatible target")
			}
			if len(e.resolve) != 0 || len(e.requests) != 0 {
				t.Error("invalid target reached online authority before preflight rejection")
			}
			if original != nil {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, original) {
					t.Error("rejected target was changed")
				}
			}
		})
	}
}

func TestPhase4JoinDeletionAndPermissionCorrectionsAreChanges(t *testing.T) {
	for _, name := range []string{"stale-backend-fields", "permission-only"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trusted.toml")
			in := joinInput(path)
			in.Session, in.Instance = "", ""
			e := &joinExecutor{resolveID: [2]string{"ws_selected", "prj_selected"}}
			e.result = verifiedWhoami("ws_selected", "prj_selected", in)
			if _, err := setup.Join(context.Background(), in, e); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if name == "stale-backend-fields" {
				raw = append(raw, []byte("oauth_client_id = 'obsolete-client'\noauth_client_secret = 'obsolete-app-secret'\noauth_device_client_secret = 'obsolete-device-secret'\n")...)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			result, err := setup.Join(context.Background(), in, e)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed {
				t.Error("deletion/permission correction did not count as changed")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Error("joined config is not private 0600")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]any
			if err := toml.Unmarshal(after, &values); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"oauth_client_id", "oauth_client_secret", "oauth_device_client_secret"} {
				if _, ok := values[key]; ok {
					t.Errorf("stale backend field %s survived publication", key)
				}
			}
			again, err := setup.Join(context.Background(), in, e)
			if err != nil || again.Changed {
				t.Error("identical private route is not an idempotent no-op")
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(unchanged, after) {
				t.Error("idempotent join rewrote config bytes")
			}
		})
	}
}

func TestPhase4JoinPublishesCompleteOAuthPair(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint("complete=", complete), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trusted.toml")
			in := joinInput(path)
			in.CredentialSource = "oauth"
			in.Account, in.ImpersonateServiceAccount = "", ""
			in.OAuthClientID = "browser.apps.googleusercontent.com"
			in.Audience = in.OAuthClientID
			in.OAuthDeviceClientID = "device.apps.googleusercontent.com"
			in.OAuthDeviceClientSecret = "synthetic-device-app-secret"
			if complete {
				in.OAuthClientSecret = "synthetic-browser-app-secret"
			}
			e := &joinExecutor{resolveID: [2]string{"ws_selected", "prj_selected"}}
			e.result = verifiedWhoami("ws_selected", "prj_selected", in)
			_, err := setup.Join(context.Background(), in, e)
			if !complete {
				if err == nil {
					t.Error("join published OAuth route without active matching app credential")
				}
				if len(e.resolve) != 0 || len(e.requests) != 0 {
					t.Error("incomplete backend reached authority")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Error("incomplete backend published config")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]any
			if err := toml.Unmarshal(raw, &values); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"oauth_client_id": in.OAuthClientID, "audience": in.Audience, "oauth_client_secret": in.OAuthClientSecret, "oauth_device_client_id": in.OAuthDeviceClientID, "oauth_device_client_secret": in.OAuthDeviceClientSecret} {
				if values[key] != want {
					t.Errorf("published %s differs from selected pair (values suppressed)", key)
				}
			}
		})
	}
}

func TestPhase4JoinOptionalDevicePairIsConsistent(t *testing.T) {
	for _, pair := range []struct {
		name, id, secret string
		valid            bool
	}{
		{name: "absent", valid: true},
		{name: "id-only", id: "device.apps.googleusercontent.com"},
		{name: "secret-only", secret: "synthetic-device-app-secret"},
		{name: "complete", id: "device.apps.googleusercontent.com", secret: "synthetic-device-app-secret", valid: true},
	} {
		t.Run(pair.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trusted.toml")
			in := joinInput(path)
			in.CredentialSource = "oauth"
			in.Account, in.ImpersonateServiceAccount = "", ""
			in.OAuthClientID = "browser.apps.googleusercontent.com"
			in.Audience = in.OAuthClientID
			in.OAuthClientSecret = "synthetic-browser-app-secret"
			in.OAuthDeviceClientID, in.OAuthDeviceClientSecret = pair.id, pair.secret
			e := &joinExecutor{resolveID: [2]string{"ws_selected", "prj_selected"}}
			e.result = verifiedWhoami("ws_selected", "prj_selected", in)
			_, err := setup.Join(context.Background(), in, e)
			if pair.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Error("incomplete optional device pair accepted")
			}
			if len(e.resolve) != 0 || len(e.requests) != 0 {
				t.Error("incomplete optional device pair reached authority")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("incomplete optional device pair published config")
			}
		})
	}
}
