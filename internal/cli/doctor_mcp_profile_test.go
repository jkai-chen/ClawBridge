package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/clirender"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/doctor"

	"github.com/pelletier/go-toml/v2"
	"github.com/zalando/go-keyring"
)

// An entry `mcp install --linear-profile` wrote passes doctor run without a
// profile, which takes the entry's own well-formed one, and with the same
// one by --mcp-linear-profile or --linear-profile; a different one conflicts
// and says which, --mcp-linear-profile winning over --linear-profile.
func TestDoctorAcceptsAnEntryInstalledWithALinearProfile(t *testing.T) {
	const profile, other = "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, agent := range []string{"claude-code", "codex"} {
		t.Run(agent, func(t *testing.T) {
			dir := isolated(t)
			config := filepath.Join(dir, "agent-config")
			scope := []string{"--workspace", "w", "--project", "p"}
			install := append(append([]string{}, scope...), "--session", "agent", "--linear-profile", profile, "mcp", "install", "--agent", agent, "--config", config)
			if agent == "claude-code" {
				install = append(install, "--hooks", "none")
			}
			var out, errs bytes.Buffer
			if err := Run(context.Background(), install, &out, &errs); err != nil {
				t.Fatal(err, errs.String())
			}
			check := func(extra ...string) doctor.Check {
				t.Helper()
				args := append(append(append([]string{}, scope...), "doctor", "--mcp-agent", agent, "--mcp-config", config, "--mcp-session", "agent"), extra...)
				var out bytes.Buffer
				ctx := clirender.WithMode(context.Background(), clirender.JSON)
				_ = Run(ctx, args, &out, &bytes.Buffer{})
				var report doctor.Report
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatalf("%v: %v %s", args, err, out.String())
				}
				for _, c := range report.Checks {
					if c.ID == "mcp."+agent+".clawbridge" {
						return c
					}
				}
				t.Fatalf("%v: no MCP check in %s", args, out.String())
				return doctor.Check{}
			}
			for _, extra := range [][]string{nil, {"--mcp-linear-profile", profile}, {"--linear-profile", profile}} {
				if c := check(extra...); c.Status != doctor.Pass {
					t.Fatalf("%v: a correct install flagged: %+v", extra, c)
				}
			}
			for _, extra := range [][]string{{"--mcp-linear-profile", other}, {"--linear-profile", other}, {"--linear-profile", profile, "--mcp-linear-profile", other}} {
				if c := check(extra...); c.Status != doctor.Fail || !strings.Contains(c.Message, "Linear profile "+profile+", expected "+other) {
					t.Fatalf("%v: a different profile accepted or unexplained: %+v", extra, c)
				}
			}
		})
	}
}

// The field case end to end: an entry mcp install wrote from another
// profile (CLAWBRIDGE_CONFIG or --client-config), copied to its own file,
// passes a doctor run without a profile that resolves the default one with
// the same authority, and names what it took from the entry. Before, the
// entry's --client-config (and that profile's cache, device and key store)
// failed as an unexplained route conflict.
func TestDoctorAcceptsAnEntryInstalledFromAnotherProfile(t *testing.T) {
	// Written with the TOML encoder the product uses: a Windows path in a
	// hand-quoted basic string would read its backslashes as escapes.
	profile := func(home, name, device, store string) []byte {
		b, err := toml.Marshal(map[string]string{
			"mode": "cloud", "server": "https://cb.example.test", "workspace": "acme", "project": "p",
			"principal": "ada@example.com", "credential_source": "device", "device_id": device,
			"key_store": store, "cache": filepath.Join(home, name+".cache.db"),
		})
		if err != nil {
			panic(err)
		}
		return b
	}
	for _, how := range []string{"env", "flag", "same-profile-elsewhere", "no-key-here"} {
		t.Run(how, func(t *testing.T) {
			dir := isolated(t)
			// The other profile's device has its key in this machine's
			// keyring, as after a join; for no-key-here it doesn't.
			keyring.MockInit()
			key, theirDevice, err := devicesign.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			if how != "no-key-here" {
				if err := devicesign.Save(devicesign.Location{Store: devicesign.StoreKeyring, Host: "cb.example.test", DeviceID: theirDevice}, key); err != nil {
					t.Fatal(err)
				}
			}
			home := filepath.Join(dir, "state")
			if err := os.MkdirAll(filepath.Join(home, "profiles"), 0700); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(home, "profiles", "other.toml")
			own := profile(home, "config", "dev_0123456789abcdefghjkmnpqrs", "file")
			theirs := profile(home, "other", theirDevice, "keyring")
			if how == "same-profile-elsewhere" {
				theirs = own
			}
			if os.WriteFile(filepath.Join(home, "config.toml"), own, 0600) != nil || os.WriteFile(other, theirs, 0600) != nil {
				t.Fatal("profiles not written")
			}
			installed := filepath.Join(dir, "installed.json")
			args := []string{"--session", "agent", "--instance", "inst_1", "--linear-profile", "lin_aaaaaaaa", "mcp", "install", "--agent", "claude-code", "--config", installed, "--hooks", "none"}
			if how == "env" {
				t.Setenv("CLAWBRIDGE_CONFIG", other)
			} else {
				args = append([]string{"--client-config", other}, args...)
			}
			var errs bytes.Buffer
			if err := Run(context.Background(), args, &bytes.Buffer{}, &errs); err != nil {
				t.Fatal(err, errs.String())
			}
			t.Setenv("CLAWBRIDGE_CONFIG", "")
			raw, err := os.ReadFile(installed)
			if err != nil {
				t.Fatal(err)
			}
			copied := filepath.Join(dir, "copy.json")
			if err := os.WriteFile(copied, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			_ = Run(clirender.WithMode(context.Background(), clirender.JSON), []string{"doctor", "--mcp-agent", "claude-code", "--mcp-config", copied, "--mcp-session", "agent", "--mcp-instance", "inst_1"}, &out, &bytes.Buffer{})
			var report doctor.Report
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err, out.String())
			}
			want := []string{"--client-config " + other}
			if how != "same-profile-elsewhere" {
				want = append(want, "--cache "+filepath.Join(home, "other.cache.db"), "--device-id "+theirDevice, "--key-store keyring", "Its device key is in that store; which member the device signs as is not checked here.")
			}
			for _, c := range report.Checks {
				if c.ID != "mcp.claude-code.clawbridge" {
					continue
				}
				if how == "no-key-here" {
					// An entry whose device has no key on this machine can't
					// sign: a failure naming the device and its store.
					if c.Status != doctor.Fail || !strings.Contains(c.Message, "device "+theirDevice+" has no usable key in its keyring store") {
						t.Fatalf("an entry with no key here passed: %+v", c)
					}
					return
				}
				if c.Status != doctor.Pass {
					t.Fatalf("a working entry flagged: %+v", c)
				}
				for _, w := range want {
					if !strings.Contains(c.Message, w) {
						t.Fatalf("%q not named: %s", w, c.Message)
					}
				}
				return
			}
			t.Fatalf("no MCP check in %s", out.String())
		})
	}
}
