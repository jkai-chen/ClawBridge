package devicekey

import (
	"strings"
	"testing"
)

func TestLinkRoundTrip(t *testing.T) {
	link, err := FormatLink("https://CB.Example.com/", "acme.eu", "ada@example.com", "cbj4k7pwq9d2mxht6rb")
	if err != nil || link != "https://cb.example.com/join#cbj1.acme.eu.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB" {
		t.Fatal(link, err)
	}
	got, err := ParseLink("  " + link + "\n")
	if err != nil || got != (Link{Origin: "https://cb.example.com", Workspace: "acme.eu", Email: "ada@example.com", Code: "CBJ-4K7PWQ9D2MXHT6RB"}) {
		t.Fatalf("%+v %v", got, err)
	}
	// Case and dashes in the code are tolerated, as when typed.
	if got, err = ParseLink("https://cb.example.com/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t.cbj-4k7p-wq9d-2mxh-t6rb"); err != nil || got.Code != "CBJ-4K7PWQ9D2MXHT6RB" {
		t.Fatal(got, err)
	}
	if got, err = ParseLink("http://127.0.0.1:8080/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB"); err != nil || got.Origin != "http://127.0.0.1:8080" {
		t.Fatal(got, err)
	}
}

func TestLinkRefusals(t *testing.T) {
	const tail = "#cbj1.acme.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB"
	for _, bad := range []string{
		"CBJ-4K7P-WQ9D-2MXH-T6RB",
		"http://cb.example.com/join" + tail,
		"ftp://cb.example.com/join" + tail,
		"https://ada:pw@cb.example.com/join" + tail,
		"https://cb.example.com/join?x=1" + tail,
		"https://cb.example.com/other" + tail,
		"https://cb.example.com/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t",
		"https://cb.example.com/join#cbj1.acme.bm90LWFuLWVtYWls.CBJ-4K7P-WQ9D-2MXH-T6RB",
		"https://cb.example.com/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t.CBJ-NOPE",
		"https://cb.example.com/join#cbj1..YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB",
		"https://cb.example.com/join#xyz.acme.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB",
	} {
		if got, err := ParseLink(bad); err == nil {
			t.Errorf("parsed %q as %+v", bad, got)
		}
	}
	if _, err := ParseLink("https://cb.example.com/join#cbj2.acme.x.y"); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatal(err)
	}
	if _, err := FormatLink("http://cb.example.com", "acme", "ada@example.com", "CBJ-4K7P-WQ9D-2MXH-T6RB"); err == nil {
		t.Fatal("formatted an http link to a remote host")
	}
}
