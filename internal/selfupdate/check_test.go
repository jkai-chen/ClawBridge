package selfupdate

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCheckMarkers(t *testing.T) {
	cases := []struct {
		name         string
		manual, hold bool
	}{{"plain", false, false}, {"manual", true, false}, {"hold", false, true}, {"both", true, true}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.manual {
				e.gh.files["/releases/download/v1.3.0/MANUAL-UPDATE-REQUIRED"] = []byte("v1.3.0 needs a manual update: sign in again.\n")
			}
			if c.hold {
				e.gh.files["/releases/download/v1.3.0/AUTO-UPDATE-HOLD"] = []byte("held\n")
			}
			got, err := Check(context.Background(), e.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.Action != Update || got.Target != "v1.3.0" || got.ManualRequired != c.manual || got.Hold != c.hold {
				t.Fatalf("Check = %+v", got)
			}
			if c.manual && !strings.HasPrefix(got.ManualText, "v1.3.0 needs a manual update") {
				t.Errorf("manual text = %q", got.ManualText)
			}
			if !c.manual && got.ManualText != "" {
				t.Errorf("manual text = %q", got.ManualText)
			}
		})
	}
}

func TestCheckUpToDateReadsNoMarkers(t *testing.T) {
	e := newEnv(t)
	e.opts.Current = "v1.3.0"
	before := e.gh.requests.Load()
	got, err := Check(context.Background(), e.opts)
	if err != nil || got.Action != UpToDate {
		t.Fatalf("Check = %+v, %v", got, err)
	}
	if n := e.gh.requests.Load() - before; n != 1 {
		t.Errorf("up to date made %d requests, want only the latest redirect", n)
	}
}

func TestCheckMarkerServerError(t *testing.T) {
	e := newEnv(t)
	e.gh.handlers["/releases/download/v1.3.0/MANUAL-UPDATE-REQUIRED"] = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	if _, err := Check(context.Background(), e.opts); err == nil {
		t.Fatal("a failing marker read was taken as no marker")
	}
}
