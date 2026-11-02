package keyagent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func frame(payload []byte) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return b
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("got %v, want code %s", err, code)
	}
	if e.Msg != messages[code] {
		t.Fatalf("message %q is not the fixed one", e.Msg)
	}
}

func TestReadRequestRefusals(t *testing.T) {
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, MaxFrame+1)
	cases := []struct {
		name string
		in   []byte
		code string
	}{
		{"oversize length", big, CodeTooLarge},
		{"truncated header", []byte{0, 0}, CodeBadRequest},
		{"truncated body", append([]byte{0, 0, 0, 20}, `{"v":1`...), CodeBadRequest},
		{"zero length", frame(nil), CodeBadRequest},
		{"not json", frame([]byte("nope")), CodeBadRequest},
		{"trailing data", frame([]byte(`{"v":1,"op":"status"} {}`)), CodeBadRequest},
		{"bad version", frame([]byte(`{"v":2,"op":"status"}`)), CodeBadVersion},
		{"no version", frame([]byte(`{"op":"status"}`)), CodeBadVersion},
		{"unknown op", frame([]byte(`{"v":1,"op":"rewrap"}`)), CodeUnknownOp},
		{"unknown field", frame([]byte(`{"v":1,"op":"status","x":1}`)), CodeBadRequest},
		{"raw digest to sign", frame([]byte(`{"v":1,"op":"sign","digest":"AAAA"}`)), CodeBadRequest},
		{"field the op does not take", frame([]byte(`{"v":1,"op":"status","kind":"linear"}`)), CodeBadRequest},
		{"passphrase on unseal", frame([]byte(`{"v":1,"op":"unseal","passphrase":"AAAA"}`)), CodeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readRequest(bytes.NewReader(c.in))
			wantCode(t, err, c.code)
		})
	}
}

func TestRefusalNeverEchoesTheRequest(t *testing.T) {
	_, err := readRequest(bytes.NewReader(frame([]byte(`{"v":1,"op":"SECRETOP","passphrase":"hunter2hunter2"}`))))
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "hunter") {
		t.Fatalf("%v", err)
	}
}

func TestRequestRoundTrips(t *testing.T) {
	b, _ := json.Marshal(request{V: 1, Op: OpSeal, Kind: "linear", ID: "lin_00000000000000000000000000000000", Plaintext: []byte("x")})
	r, err := readRequest(bytes.NewReader(frame(b)))
	if err != nil || r.Op != OpSeal || string(r.Plaintext) != "x" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestEveryErrorCodeHasAFixedMessage(t *testing.T) {
	for _, c := range []string{CodeLocked, CodeBadRequest, CodeBadVersion, CodeUnknownOp, CodeTooLarge, CodeForbidden,
		CodePurposeRequired, CodeInvalid, CodeNotFound, CodeExists, CodeWrongPass, CodeInternal} {
		if newError(c).Msg == "" {
			t.Fatalf("%s has no message", c)
		}
	}
	if !errors.Is(newError(CodeLocked), ErrLocked) || ErrLocked.Error() != "ClawBridge is locked: ask your person to run `clawbridge keys unlock` in their own terminal." {
		t.Fatal("ErrLocked text")
	}
}

func TestWriteFrameRefusesOversize(t *testing.T) {
	if writeFrame(&bytes.Buffer{}, make([]byte, MaxFrame+1)) == nil {
		t.Fatal("oversize frame written")
	}
}

func FuzzFrame(f *testing.F) {
	f.Add(frame([]byte(`{"v":1,"op":"status"}`)))
	f.Add(frame([]byte(`{"v":1,"op":"sign","method":"GET","origin":"https://a.example.test","path":"/x","body_sha256":"00","timestamp":"1","device_id":"d"}`)))
	f.Add(frame([]byte(`{"v":1,"op":"seal","kind":"linear","plaintext":"AAAA"}`)))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 5, '{'})
	f.Fuzz(func(t *testing.T, in []byte) {
		req, err := readRequest(bytes.NewReader(in))
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Msg != messages[e.Code] {
				t.Fatalf("not a fixed error: %v", err)
			}
			return
		}
		if req.V != Version {
			t.Fatal("accepted a wrong version")
		}
		if _, ok := ops[req.Op]; !ok {
			t.Fatal("accepted an unknown op")
		}
		if req.set()&^ops[req.Op] != 0 {
			t.Fatal("accepted a field the op does not take")
		}
	})
}

func TestSignFieldChecks(t *testing.T) {
	if !validOrigin("https://a.example.test") || !validOrigin("http://127.0.0.1:8080") {
		t.Fatal("good origins refused")
	}
	for _, o := range []string{"", "ftp://a", "https://", "https://a/b", "https://a?x", "https://u@a", "https://a\n", "https://a b", "a.example.test"} {
		if validOrigin(o) {
			t.Fatalf("origin %q accepted", o)
		}
	}
	if printable("/a\nb", 10) || printable("/a b", 10) || printable("", 10) || printable("/é", 10) || printable(strings.Repeat("a", 11), 10) || !printable("/v1/x?y=1", 20) {
		t.Fatal("printable")
	}
	if isDigits("12a", 5) || isDigits("", 5) || isDigits("123456", 5) || !isDigits("1700000000", 12) {
		t.Fatal("isDigits")
	}
	if isLowerHex("ABCD", 4) || isLowerHex("abc", 4) || !isLowerHex("abcd", 4) {
		t.Fatal("isLowerHex")
	}
	if validMethod("get") || validMethod("GET\n") || !validMethod("POST") {
		t.Fatal("validMethod")
	}
}
