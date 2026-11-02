package agentenv

import "testing"

func TestFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{nil, ""},
		{[]string{"PATH=/bin", "HOME=/h"}, ""},
		{[]string{"CLAUDECODE=1"}, "CLAUDECODE"},
		{[]string{"CODEX_SANDBOX=seatbelt"}, "CODEX_SANDBOX"},
		{[]string{"CODEX_THREAD_ID=x"}, "CODEX_THREAD_ID"},
		{[]string{"CLAWBRIDGE_SESSION=s"}, "CLAWBRIDGE_SESSION"},
		{[]string{"CLAWBRIDGE_INSTANCE=i"}, "CLAWBRIDGE_INSTANCE"},
		{[]string{"CODEX=1", "CLAUDE=1", "CLAWBRIDGE_OUTPUT=json"}, ""},
	} {
		if got := FromEnv(tc.env); got != tc.want {
			t.Errorf("FromEnv(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestMarkerReadsEnviron(t *testing.T) {
	old := Environ
	defer func() { Environ = old }()
	Environ = func() []string { return []string{"CLAUDECODE=1"} }
	if Marker() != "CLAUDECODE" {
		t.Fatal("marker not read")
	}
	Environ = func() []string { return nil }
	if Marker() != "" {
		t.Fatal("empty env must give none")
	}
}

func TestDisplayNamesOnlyKnownVariables(t *testing.T) {
	for in, want := range map[string]string{
		"CLAUDECODE":         "CLAUDECODE",
		"CLAWBRIDGE_SESSION": "CLAWBRIDGE_SESSION",
		"CODEX_HOME":         "CODEX_*",
		"AWS_SECRET":         "an agent environment variable",
		"":                   "an agent environment variable",
	} {
		if got := Display(in); got != want {
			t.Errorf("Display(%q) = %q, want %q", in, got, want)
		}
	}
}
