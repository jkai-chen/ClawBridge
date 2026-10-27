package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The shared contract (api/dispatcher-model/golden): the server's Go tests
// write its launcher answers there, and the launcher client must decode
// each one, so a drift on either side fails CI.
func TestLauncherClientDecodesTheGoldenAnswers(t *testing.T) {
	dir := filepath.Join("..", "..", "api", "dispatcher-model", "golden")
	files := map[string]string{
		"/v1/dispatcher/launcher/wanted":     "launcher-wanted.json",
		"/v1/dispatcher/launcher/credential": "launcher-credential.json",
		"/v1/dispatcher/launcher/report":     "launcher-report.json",
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(dir, files[r.URL.Path]))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	c := NewLauncherClient(srv.URL, testToken).(*launcherClient)
	c.http.Transport = srv.Client().Transport
	ctx := context.Background()

	wanted, max, err := c.Wanted(ctx)
	if err != nil || len(wanted) == 0 || max < 1 {
		t.Fatalf("wanted: %+v %d %v", wanted, max, err)
	}
	for _, w := range wanted {
		if !ownerRefPattern.MatchString(w.Ref) || w.Provider == "" || w.Revision < 1 {
			t.Fatalf("a wanted agent the reconciler would refuse: %+v", w)
		}
	}
	if cred, err := c.Credential(ctx, wanted[0].Ref); err != nil || !credentialPattern.MatchString(cred) {
		t.Fatalf("credential: %v", err)
	}
	if err := c.Report(ctx, wanted[0].Ref, "started"); err != nil {
		t.Fatalf("report: %v", err)
	}
}
