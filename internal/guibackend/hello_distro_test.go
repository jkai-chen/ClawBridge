package guibackend

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func helloBody(t *testing.T, platform string) string {
	t.Helper()
	s := &Server{Hello: Hello{ClientVersion: "1.0.0-test", Platform: platform}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestHelloNamesTheWSLDistroOnlyOnWSL(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	if b := helloBody(t, "wsl"); !strings.Contains(b, `"distro":"Ubuntu"`) {
		t.Fatalf("wsl hello: %s", b)
	}
	if b := helloBody(t, "linux"); strings.Contains(b, "distro") {
		t.Fatalf("linux hello names a distro: %s", b)
	}
	t.Setenv("WSL_DISTRO_NAME", "")
	if b := helloBody(t, "wsl"); strings.Contains(b, "distro") {
		t.Fatalf("empty distro is not omitted: %s", b)
	}
}
