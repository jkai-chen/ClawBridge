package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleaseVersionHasNoRuntimeDependencies(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, injected := range []bool{false, true} {
		name := "source-defaults"
		if injected {
			name = "injected"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "clawbridge")
			args := []string{"build", "-o", binary}
			want := map[string]any{"version": "dev", "commit": "unknown", "build_time": "unknown"}
			if injected {
				args = append(args, "-ldflags", "-X clawbridge/internal/buildinfo.Version=v0.1.0 -X clawbridge/internal/buildinfo.Commit="+commit+" -X clawbridge/internal/buildinfo.BuildTime=2026-10-21T00:00:00Z")
				want = map[string]any{"version": "v0.1.0", "commit": commit, "build_time": "2026-10-21T00:00:00Z"}
			}
			args = append(args, "./cmd/clawbridge")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			build := exec.CommandContext(ctx, "go", args...)
			build.Dir = root
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build version fixture: %v %s", err, out)
			}
			home := filepath.Join(dir, "isolated home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			poison := []byte("invalid = [\nPRIVATE-CONFIG-SENTINEL\n")
			for _, path := range []string{filepath.Join(home, ".clawbridge.toml"), filepath.Join(dir, ".clawbridge.toml"), filepath.Join(home, "state-is-a-file")} {
				if err := os.WriteFile(path, poison, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected request", 500)
			}))
			defer server.Close()
			env := []string{}
			for _, v := range os.Environ() {
				key := strings.SplitN(v, "=", 2)[0]
				if strings.HasPrefix(key, "CLAWBRIDGE_") || key == "HOME" || key == "XDG_CONFIG_HOME" || key == "XDG_DATA_HOME" || key == "XDG_CACHE_HOME" {
					continue
				}
				env = append(env, v)
			}
			env = append(env, "HOME="+home, "CLAWBRIDGE_HOME="+filepath.Join(home, "state-is-a-file"), "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home, "XDG_CACHE_HOME="+home, "CLAWBRIDGE_MODE=cloud", "CLAWBRIDGE_SERVER="+server.URL, "CLAWBRIDGE_AUDIENCE=missing-credential", "CLAWBRIDGE_CREDENTIAL_SOURCE=oauth", "CLAWBRIDGE_DB="+filepath.Join(home, "never-create.db"))
			before := releaseVersionFiles(t, dir)
			for _, argv := range [][]string{{"version"}, {"--version"}, {"version", "extra"}, {"--version", "extra"}} {
				cmd := exec.CommandContext(ctx, binary, argv...)
				cmd.Dir = dir
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if len(argv) > 1 {
					if err == nil {
						t.Fatalf("extra arguments accepted: %v", argv)
					}
				} else {
					if err != nil {
						t.Fatalf("%v depends on runtime: %v %s", argv, err, out)
					}
					var got map[string]any
					if err := json.Unmarshal(out, &got); err != nil {
						t.Fatalf("invalid version JSON: %v %s", err, out)
					}
					if len(got) != 2 || got["source"] != "build" || !reflect.DeepEqual(got["data"], want) {
						t.Fatalf("version shape/metadata: %v", got)
					}
				}
				if strings.Contains(string(out), "PRIVATE-CONFIG-SENTINEL") {
					t.Fatal("version exposed config")
				}
			}
			if requests.Load() != 0 {
				t.Fatal("version contacted configured authority")
			}
			if after := releaseVersionFiles(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("version mutated isolated files")
			}
		})
	}
}
func releaseVersionFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
