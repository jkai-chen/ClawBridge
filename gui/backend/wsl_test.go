package backend

import (
	"errors"
	"reflect"
	"testing"
	"unicode/utf16"
)

func utf16le(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func TestDecodeWSLList(t *testing.T) {
	want := []string{"Ubuntu-24.04", "Debian", "docker-desktop"}
	cases := map[string][]byte{
		"utf-16le with a BOM, CRLF":    utf16le("Ubuntu-24.04\r\nDebian\r\ndocker-desktop\r\n", true),
		"utf-16le without a BOM":       utf16le("Ubuntu-24.04\r\nDebian\r\ndocker-desktop\r\n", false),
		"utf-16le, no final newline":   utf16le("Ubuntu-24.04\r\nDebian\r\ndocker-desktop", true),
		"utf-16le with blank lines":    utf16le("Ubuntu-24.04\r\n\r\nDebian\r\n \r\ndocker-desktop\r\n", true),
		"utf-8 (WSL_UTF8=1)":           []byte("Ubuntu-24.04\r\nDebian\r\ndocker-desktop\r\n"),
		"utf-8 with LF only":           []byte("Ubuntu-24.04\nDebian\ndocker-desktop\n"),
		"utf-16le, NUL after each row": append(utf16le("Ubuntu-24.04\r\nDebian\r\ndocker-desktop\r\n", true), 0, 0),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if got := DecodeWSLList(raw); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
	if got := DecodeWSLList(utf16le("Ünïcode-Distro\r\n", true)); !reflect.DeepEqual(got, []string{"Ünïcode-Distro"}) {
		t.Fatalf("non-ASCII names decode: %q", got)
	}
	if got := DecodeWSLList(nil); len(got) != 0 {
		t.Fatalf("empty output: %q", got)
	}
}

func TestWSLArgvIsFixed(t *testing.T) {
	// GUI-DESIGN §6.2: only the distro varies; every other word is fixed.
	got := WSLArgv("Ubuntu-24.04")
	want := []string{
		"wsl.exe", "-d", "Ubuntu-24.04", "--cd", "~", "--", "sh", "-c",
		`L=$(getent passwd "$(id -un)" | cut -d: -f7); exec "${L:-/bin/sh}" -lc "exec clawbridge gui-backend --stdio"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	// A hostile name stays one argv element and never reaches the script.
	hostile := WSLArgv("x; rm -rf ~")
	if hostile[2] != "x; rm -rf ~" || hostile[8] != want[8] || len(hostile) != len(want) {
		t.Fatalf("the distro must be one element: %q", hostile)
	}
}

func TestCheckDistro(t *testing.T) {
	list := []string{"Ubuntu-24.04", "Debian"}
	if d, err := CheckDistro("Debian", list); err != nil || d != "Debian" {
		t.Fatalf("listed distro: %q %v", d, err)
	}
	for _, bad := range []string{"", "debian", "Debian ", "--exec", "-d", "Fedora"} {
		if _, err := CheckDistro(bad, append(list, "--exec", "-d")); !errors.Is(err, ErrUnknownDistro) {
			t.Errorf("%q: got %v", bad, err)
		}
	}
}
