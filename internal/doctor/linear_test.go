package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/install"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLinearInstalledProfileMatchesDoctorExactly(t *testing.T) {
	for _, agent := range []string{"codex", "claude-code"} {
		for _, profile := range []string{"", "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
			t.Run(agent+"-"+profile, func(t *testing.T) {
				dir := t.TempDir()
				command := filepath.Join(dir, "clawbridge")
				if err := os.WriteFile(command, []byte("fixture"), 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "agent-config")
				db := filepath.Join(dir, "state.db")
				_, err := install.Install(install.Options{Agent: agent, Config: path, Command: command, DB: db, Mode: "local", Workspace: "w", Project: "p", Session: "actor", Instance: "instance", LinearProfile: profile})
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				expected := MCPConfig{Agent: agent, Name: "clawbridge", Path: path, DB: db, Mode: "local", Workspace: "w", Project: "p", Session: "actor", Instance: "instance", LinearProfile: profile}
				checkMCP(expected, func(check Check) {
					if check.Status != Pass {
						t.Fatal("matching native profile rejected", check)
					}
				})
				if profile != "" {
					for _, wrong := range []string{"", "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
						expected.LinearProfile = wrong
						checkMCP(expected, func(check Check) {
							if check.Status != Fail {
								t.Fatal("changed or omitted native profile accepted", check)
							}
						})
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("doctor modified selected config", err)
				}
			})
		}
	}
	if _, err := expectedMCPEntry(MCPConfig{LinearProfile: "secret\nvalue"}); err == nil {
		t.Fatal("hostile profile accepted")
	}
}

func TestLinearCapabilitiesAreVisibleInDoctor(t *testing.T) {
	cases := map[string]struct {
		caps   linearprovider.Capabilities
		err    error
		status string
		want   []string
	}{
		"on":         {allCapabilities(), nil, Pass, []string{"label_eligibility_by_filter=on", "parent_same_destination=on", "label_membership_proof=on"}},
		"off":        {linearprovider.Capabilities{ParentSameDestination: true, Reasons: map[string]string{"label_eligibility_by_filter": "list_issue_labels schema changed: includeGroups missing or no longer a boolean defaulting to false"}}, nil, Warn, []string{"label_eligibility_by_filter=off", "includeGroups"}},
		"unreadable": {linearprovider.Capabilities{}, fmt.Errorf("dial secret transport detail: %w", &linearprovider.Error{Kind: "unavailable"}), Warn, []string{"could not be read (unavailable)"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var checks []Check
			checkLinearCapabilities(context.Background(), func(context.Context) (linearprovider.Capabilities, error) { return tc.caps, tc.err }, func(c Check) { checks = append(checks, c) })
			if len(checks) != 1 || checks[0].ID != "linear.capabilities" || checks[0].Status != tc.status {
				t.Fatal(checks)
			}
			for _, want := range tc.want {
				if !strings.Contains(checks[0].Message, want) {
					t.Fatalf("message %q lacks %q", checks[0].Message, want)
				}
			}
			if strings.Contains(checks[0].Message, "secret") {
				t.Fatal("transport detail leaked", checks[0].Message)
			}
		})
	}
	report := Run(context.Background(), Input{}, nil)
	for _, check := range report.Checks {
		if check.ID == "linear.capabilities" {
			t.Fatal("Linear section shown without a configured profile")
		}
	}
}

// allCapabilities turns every boolean capability on, found through its JSON
// form so the test does not repeat the field list.
func allCapabilities() linearprovider.Capabilities {
	var caps linearprovider.Capabilities
	raw, _ := json.Marshal(caps)
	fields := map[string]any{}
	json.Unmarshal(raw, &fields)
	for key, value := range fields {
		if _, ok := value.(bool); ok {
			fields[key] = true
		}
	}
	raw, _ = json.Marshal(fields)
	json.Unmarshal(raw, &caps)
	return caps
}

func TestDoctorListsEveryLinearCapabilityWithItsOffReason(t *testing.T) {
	all := allCapabilities()
	raw, _ := json.Marshal(all)
	fields := map[string]any{}
	json.Unmarshal(raw, &fields)
	keys := []string{}
	for key, value := range fields {
		if _, ok := value.(bool); ok {
			keys = append(keys, key)
		}
	}
	if len(keys) < 12 {
		t.Fatalf("capability fixture lost flags: %v", keys)
	}
	run := func(caps linearprovider.Capabilities) Check {
		var checks []Check
		checkLinearCapabilities(context.Background(), func(context.Context) (linearprovider.Capabilities, error) { return caps, nil }, func(c Check) { checks = append(checks, c) })
		if len(checks) != 1 {
			t.Fatal(checks)
		}
		return checks[0]
	}
	if check := run(all); check.Status != Pass || strings.Contains(check.Message, "=off") {
		t.Fatal("all capabilities on", check)
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			fields := map[string]any{}
			json.Unmarshal(raw, &fields)
			fields[key] = false
			// A reason left on a flag that is on is stale and must not show.
			stale := keys[0]
			if stale == key {
				stale = keys[1]
			}
			fields["reasons"] = map[string]string{key: "synthetic reason for " + key, stale: "stale"}
			b, _ := json.Marshal(fields)
			var caps linearprovider.Capabilities
			if err := json.Unmarshal(b, &caps); err != nil {
				t.Fatal(err)
			}
			check := run(caps)
			if check.Status != Warn || check.Action == "" {
				t.Fatal("an off capability did not warn", check)
			}
			for _, other := range keys {
				want := other + "=on"
				if other == key {
					want = other + "=off"
				}
				if msg := ", " + check.Message; !strings.Contains(msg, ", "+want+",") && !strings.Contains(msg, ", "+want+".") {
					t.Fatalf("message %q lacks %q", check.Message, want)
				}
			}
			if !strings.Contains(check.Message, key+": synthetic reason for "+key) || strings.Contains(check.Message, "stale") {
				t.Fatalf("off reason wrong: %q", check.Message)
			}
		})
	}
}

// catalogCaller serves a fixed tool catalog and refuses every tool call.
type catalogCaller []*mcp.Tool

func (c catalogCaller) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: c}, nil
}
func (catalogCaller) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, errors.New("capability negotiation must not call a tool")
}

// The fixture holds today's native schemas (from the 68-tool catalog of
// 2026-10-22) for the tools capability negotiation reads. A capability that
// would be off against them fails here, so adding one forces a decision
// instead of a permanent doctor warning.
func TestEveryDoctorCapabilityFlagIsOnAgainstTheNativeCatalog(t *testing.T) {
	raw, err := os.ReadFile("testdata/linear-native-catalog-20261022.json")
	if err != nil {
		t.Fatal(err)
	}
	var tools []*mcp.Tool
	if err = json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	p, err := linearprovider.NewWithCaller(context.Background(), catalogCaller(tools))
	if err != nil {
		t.Fatal(err)
	}
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range capabilityFlags(caps) {
		if !flag.on {
			t.Errorf("%s is off against the native catalog: %s", flag.key, caps.Reasons[flag.key])
		}
	}
	if len(caps.Reasons) != 0 {
		t.Errorf("off reasons on a healthy catalog: %v", caps.Reasons)
	}
}
