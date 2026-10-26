package devicesign

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/devicekey"
	"github.com/zalando/go-keyring"
)

func newLocation(t *testing.T, store string) (Location, ed25519.PrivateKey) {
	t.Helper()
	key, id, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if id != devicekey.ID(key.Public().(ed25519.PublicKey)) {
		t.Fatal("the device ID isn't derived from the key")
	}
	return Location{Store: store, Host: "cb.example.test", Home: t.TempDir(), DeviceID: id}, key
}

func TestKeyringRoundTrip(t *testing.T) {
	keyring.MockInit()
	l, key := newLocation(t, StoreKeyring)
	if _, err := Load(l); !errors.Is(err, ErrNoKey) {
		t.Fatal("a missing key:", err)
	}
	if err := Save(l, key); err != nil {
		t.Fatal(err)
	}
	if raw, _ := keyring.Get(KeyringService, "cb.example.test/"+l.DeviceID); raw != EncodeKey(key) {
		t.Fatal("not stored under host/device:", raw)
	}
	back, err := Load(l)
	if err != nil || !back.Equal(key) {
		t.Fatal(err)
	}
	other, _ := newLocation(t, StoreKeyring)
	l2 := l
	l2.DeviceID = other.DeviceID
	if err := Save(l2, key); err == nil {
		t.Fatal("saved a key under another device's ID")
	}
	if err := Delete(l); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(l); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	if err := Delete(l); err != nil {
		t.Fatal("deleting a deleted key:", err)
	}
	keyring.MockInitWithError(errors.New("locked"))
	defer keyring.MockInit()
	if err := Save(l, key); err == nil || !strings.Contains(err.Error(), "--key-store file") {
		t.Fatal("a locked keyring's way out:", err)
	}
}

func TestFileStore(t *testing.T) {
	l, key := newLocation(t, StoreFile)
	if err := Save(l, key); err != nil {
		t.Fatal(err)
	}
	if back, err := Load(l); err != nil || !back.Equal(key) {
		t.Fatal(err)
	}
	if err := Save(l, key); err == nil {
		t.Fatal("overwrote a key file")
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, _ := os.Stat(l.Path())
	dir, _ := os.Stat(filepath.Dir(l.Path()))
	if info.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", info.Mode(), dir.Mode())
	}
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{l.Path(), 0o644}, {l.Path(), 0o640}, {l.Path(), 0o604}, {filepath.Dir(l.Path()), 0o755}, {filepath.Dir(l.Path()), 0o710}} {
		if err := os.Chmod(c.path, c.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(l); err == nil || !strings.Contains(err.Error(), "narrower") {
			t.Errorf("%s at %o read: %v", c.path, c.mode, err)
		}
		if err := CheckFile(l); err == nil {
			t.Errorf("doctor passed %s at %o", c.path, c.mode)
		}
		_ = os.Chmod(l.Path(), 0o600)
		_ = os.Chmod(filepath.Dir(l.Path()), 0o700)
	}
	if _, err := Load(l); err != nil {
		t.Fatal(err)
	}
	// A symlink is not a key file, even to this device's own key.
	elsewhere := l
	elsewhere.Home = t.TempDir()
	if err := Save(elsewhere, key); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(l.Path())
	if err := os.Symlink(elsewhere.Path(), l.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(l); err == nil || !strings.Contains(err.Error(), "not a plain") {
		t.Fatal("read a key through a symlink:", err)
	}
	_ = os.Remove(l.Path())
	if err := os.WriteFile(l.Path(), []byte(EncodeKey(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	other, _ := newLocation(t, StoreFile)
	link := l
	link.DeviceID = other.DeviceID
	// Another device's key under this device's name.
	swapped := l
	swapped.DeviceID = other.DeviceID
	if err := os.WriteFile(link.Path(), []byte(EncodeKey(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(swapped); err == nil || !strings.Contains(err.Error(), "another device") {
		t.Fatal(err)
	}
}

func TestEnvKey(t *testing.T) {
	l, key := newLocation(t, StoreEnv)
	t.Setenv(EnvKey, "")
	if _, err := Load(l); err == nil {
		t.Fatal("loaded an unset key")
	}
	t.Setenv(EnvKey, EncodeKey(key)+"\n")
	if back, err := Load(l); err != nil || !back.Equal(key) {
		t.Fatal(err)
	}
	t.Setenv(EnvKey, "not-a-key")
	if _, err := Load(l); err == nil {
		t.Fatal("loaded a malformed key")
	}
	if err := Save(l, key); err == nil {
		t.Fatal("saved to the environment")
	}
}

// fields reads a header the way the server does.
func fields(t *testing.T, header string) map[string]string {
	t.Helper()
	rest, ok := strings.CutPrefix(header, Scheme+" v1 ")
	if !ok {
		t.Fatal(header)
	}
	out := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		out[k] = v
	}
	return out
}

func TestAuthorizationSignsTheTargetAsGiven(t *testing.T) {
	_, key := newLocation(t, StoreEnv)
	s := NewSigner(key, "https://cb.example.test", 0, time.Time{})
	now := time.Unix(1761498000, 0)
	s.Now = func() time.Time { return now }
	target, body := "/v1/x%20y?q=a%20b", []byte(`{"a":1}`)
	h, err := s.Authorization("POST", target, body)
	if err != nil {
		t.Fatal(err)
	}
	f := fields(t, h)
	nonce, _ := base64.RawURLEncoding.DecodeString(f["nonce"])
	sig, _ := base64.RawURLEncoding.DecodeString(f["sig"])
	if f["device"] != s.DeviceID() || f["ts"] != "1761498000" || len(nonce) != 16 || len(f) != 4 {
		t.Fatal(f)
	}
	pub := key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, devicekey.RequestBase("POST", "https://cb.example.test", target, f["ts"], f["nonce"], body), sig) {
		t.Fatal("the signature doesn't cover the target as given")
	}
	for _, other := range [][]string{{"GET", "https://cb.example.test", target, `{"a":1}`}, {"POST", "https://evil.test", target, `{"a":1}`}, {"POST", "https://cb.example.test", "/v1/x y?q=a b", `{"a":1}`}, {"POST", "https://cb.example.test", "/v1/x%20y", `{"a":1}`}, {"POST", "https://cb.example.test", target, `{"a":2}`}} {
		if ed25519.Verify(pub, devicekey.RequestBase(other[0], other[1], other[2], f["ts"], f["nonce"], []byte(other[3])), sig) {
			t.Errorf("the signature also covers %v", other)
		}
	}
	h2, _ := s.Authorization("POST", target, body)
	if fields(t, h2)["nonce"] == f["nonce"] {
		t.Fatal("a nonce was reused")
	}
}

func TestClockOffset(t *testing.T) {
	_, key := newLocation(t, StoreEnv)
	s := NewSigner(key, "https://cb.example.test", 0, time.Time{})
	now := time.Unix(1761498000, 0)
	s.Now = func() time.Time { return now }
	off, err := s.Correct(now.Unix() + 400)
	if err != nil || off != 400*time.Second {
		t.Fatal(off, err)
	}
	h, _ := s.Authorization("POST", "/v1/execute", nil)
	if fields(t, h)["ts"] != strconv.FormatInt(now.Unix()+400, 10) {
		t.Fatal("the correction isn't applied:", h)
	}
	if _, err = s.Correct(now.Unix() + 3601); err == nil || !strings.Contains(err.Error(), "60 minutes off") {
		t.Fatal("corrected past an hour:", err)
	}
	if _, err = s.Correct(now.Unix() - 3601); err == nil {
		t.Fatal("corrected past an hour behind")
	}
	if got, _ := s.Offset(); got != 400*time.Second {
		t.Fatal("a refused correction changed the offset:", got)
	}
	if _, err = s.Correct(now.Unix() + 3600); err != nil {
		t.Fatal("refused exactly an hour:", err)
	}
	// The offset lapses after 24 h of local time.
	now = now.Add(OffsetLife)
	if got, at := s.Offset(); got != 0 || !at.IsZero() {
		t.Fatal("an offset outlived 24 h:", got)
	}
	// A stored offset is kept while fresh and in range, else dropped.
	for _, c := range []struct {
		off  time.Duration
		at   time.Time
		want time.Duration
	}{
		{90 * time.Second, time.Now().Add(-time.Hour), 90 * time.Second},
		{90 * time.Second, time.Now().Add(-25 * time.Hour), 0},
		{2 * time.Hour, time.Now(), 0},
		{90 * time.Second, time.Time{}, 0},
	} {
		if got, _ := NewSigner(key, "https://cb.example.test", c.off, c.at).Offset(); got != c.want {
			t.Errorf("stored %v at %v: %v", c.off, c.at, got)
		}
	}
}

func TestJoinProof(t *testing.T) {
	_, key := newLocation(t, StoreEnv)
	s := NewSigner(key, "https://cb.example.test", 0, time.Time{})
	ts, proof := s.JoinProof("ada@example.com", "4K7PWQ9D2MXHT6RB")
	sig, _ := base64.RawURLEncoding.DecodeString(proof)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), devicekey.JoinProofBase("https://cb.example.test", "ada@example.com", "4K7PWQ9D2MXHT6RB", ts), sig) {
		t.Fatal("the join proof doesn't verify")
	}
}
