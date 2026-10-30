package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// linear.connection names the store the personal connection uses, as
// auth.device names the device key's, and a file's mode problem.
func TestLinearConnectionCheckNamesItsStore(t *testing.T) {
	for _, c := range []struct {
		store  string
		err    error
		status string
		want   []string
	}{
		{"keyring", nil, Pass, []string{"readable from its keyring store"}},
		{"file", nil, Pass, []string{"readable from its file store", "plain file, 0600", "act as you in Linear"}},
		{"file", errors.New("Linear connection: secure credential unavailable: the Linear connection file /home/example/.clawbridge/linear/connections/x.json is mode 0644; it must be 0600 or narrower"), Warn, []string{"(file) can't be used", "mode 0644"}},
	} {
		var got Check
		checkLinearConnection(context.Background(), "lin_example", func(context.Context) (string, error) { return c.store, c.err }, func(check Check) { got = check })
		if got.ID != "linear.connection" || got.Status != c.status {
			t.Fatalf("%s %v: %+v", c.store, c.err, got)
		}
		for _, want := range c.want {
			if !strings.Contains(got.Message, want) {
				t.Fatalf("message lacks %q: %s", want, got.Message)
			}
		}
		if c.err != nil && !strings.Contains(got.Action, "--key-store") {
			t.Fatalf("action: %s", got.Action)
		}
	}
}
