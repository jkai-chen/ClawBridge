package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"sync"
	"time"
)

const (
	// maxSessions caps the live session secrets, one per launched tab; the
	// oldest is dropped past it (R1 nit a).
	maxSessions = 16
	// maxLaunches caps the launch tokens waiting to be redeemed.
	maxLaunches = 32
)

// randomHex is n random bytes as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type launch struct {
	token   string
	expires time.Time
	file    string
	timer   *time.Timer
}

// tokens holds the single-use launch tokens (C2) and the live session
// secrets (C3, C4).
type tokens struct {
	mu       sync.Mutex
	ttl      time.Duration
	launches []*launch
	sessions []string
}

func newTokens(ttl time.Duration) *tokens { return &tokens{ttl: ttl} }

// issue makes a 256-bit launch token valid for the TTL. file, if not empty,
// is a launch file that is removed when the token is redeemed, expires or is
// dropped.
func (t *tokens) issue() (string, error) {
	tok, err := randomHex(32)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l := &launch{token: tok, expires: time.Now().Add(t.ttl)}
	l.timer = time.AfterFunc(t.ttl, func() { t.expire(l) })
	t.launches = append(t.launches, l)
	for len(t.launches) > maxLaunches {
		t.drop(0)
	}
	return tok, nil
}

// attachFile records the launch file of a token.
func (t *tokens) attachFile(tok, file string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.launches {
		if l.token == tok {
			l.file = file
			return
		}
	}
	os.Remove(file)
}

// drop removes launch i; t.mu is held.
func (t *tokens) drop(i int) {
	l := t.launches[i]
	l.timer.Stop()
	if l.file != "" {
		os.Remove(l.file)
	}
	t.launches = append(t.launches[:i], t.launches[i+1:]...)
}

func (t *tokens) expire(l *launch) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, x := range t.launches {
		if x == l {
			t.drop(i)
			return
		}
	}
}

// redeem uses a launch token once and, if it was valid, returns a fresh
// session secret. A token that matches is consumed even when it has expired.
func (t *tokens) redeem(tok string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	hit := -1
	for i, l := range t.launches {
		if subtle.ConstantTimeCompare([]byte(l.token), []byte(tok)) == 1 {
			hit = i
		}
	}
	if hit < 0 {
		return "", false
	}
	fresh := time.Now().Before(t.launches[hit].expires)
	t.drop(hit)
	if !fresh {
		return "", false
	}
	secret, err := randomHex(32)
	if err != nil {
		return "", false
	}
	t.sessions = append(t.sessions, secret)
	for len(t.sessions) > maxSessions {
		t.sessions = t.sessions[1:]
	}
	return secret, true
}

// validSecret compares got against every live secret in constant time.
func (t *tokens) validSecret(got string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ok := 0
	for _, s := range t.sessions {
		ok |= subtle.ConstantTimeCompare([]byte(s), []byte(got))
	}
	return ok == 1
}

// closeAll forgets every secret and removes every launch file.
func (t *tokens) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.launches) > 0 {
		t.drop(0)
	}
	t.sessions = nil
}
