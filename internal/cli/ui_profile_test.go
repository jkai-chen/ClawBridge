package cli

import "testing"

func TestUIProfileCarriesWhoAndWhere(t *testing.T) {
	got := uiProfile(Config{Workspace: "ws_example", Project: "prj_example", Principal: "mara@example.test", DeviceID: "dev_secretish", Server: "https://s.example.test"}, true, "mara-mbp")
	want := map[string]any{"workspace": "ws_example", "project": "prj_example", "waiting_for_project": true, "principal": "mara@example.test", "device": "mara-mbp"}
	if len(got) != len(want) {
		t.Fatalf("profile %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v", k, got[k], v)
		}
	}
}
