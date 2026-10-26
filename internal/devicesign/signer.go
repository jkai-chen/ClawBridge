package devicesign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"clawbridge/internal/devicekey"
)

// Scheme is the Authorization scheme of a device-signed request.
const Scheme = "ClawBridge-Device"

const (
	// MaxOffset is the largest clock correction kept: beyond it the
	// machine's clock is wrong, not the network slow (§7).
	MaxOffset = time.Hour
	// OffsetLife is how long a measured offset is trusted (AU-L7).
	OffsetLife = 24 * time.Hour
	nonceBytes = 16
)

// Signer signs requests for one device against one server origin.
type Signer struct {
	id     string
	key    ed25519.PrivateKey
	origin string
	// Now is the local clock; tests replace it.
	Now func() time.Time

	mu       sync.Mutex
	offset   time.Duration
	offsetAt time.Time
}

// NewSigner signs as key's device for origin (scheme://host[:port], the
// server's public origin). offset and offsetAt are a stored clock
// correction, dropped when older than OffsetLife or beyond MaxOffset.
func NewSigner(key ed25519.PrivateKey, origin string, offset time.Duration, offsetAt time.Time) *Signer {
	s := &Signer{id: devicekey.ID(key.Public().(ed25519.PublicKey)), key: key, origin: origin, Now: time.Now}
	if !offsetAt.IsZero() && time.Since(offsetAt) < OffsetLife && offset <= MaxOffset && offset >= -MaxOffset {
		s.offset, s.offsetAt = offset, offsetAt
	}
	return s
}

// DeviceID is the signing device's ID.
func (s *Signer) DeviceID() string { return s.id }

// Origin is the origin the signatures cover.
func (s *Signer) Origin() string { return s.origin }

// Offset is the clock correction in use, and when it was measured; zero
// when there is none.
func (s *Signer) Offset() (time.Duration, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.offsetAt.IsZero() && s.Now().Sub(s.offsetAt) >= OffsetLife {
		s.offset, s.offsetAt = 0, time.Time{}
	}
	return s.offset, s.offsetAt
}

// Time is the server's time as this signer reckons it.
func (s *Signer) Time() time.Time {
	offset, _ := s.Offset()
	return s.Now().Add(offset)
}

// Authorization signs one request: method, the request target exactly as
// it will be sent (path and query), and the raw body. Each call has a new
// nonce, so a retry is a new signature.
func (s *Signer) Authorization(method, target string, body []byte) (string, error) {
	var n [nonceBytes]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", errors.New("cannot make a request nonce")
	}
	nonce := base64.RawURLEncoding.EncodeToString(n[:])
	ts := strconv.FormatInt(s.Time().Unix(), 10)
	sig := ed25519.Sign(s.key, devicekey.RequestBase(method, s.origin, target, ts, nonce, body))
	return fmt.Sprintf("%s v1 device=%s, ts=%s, nonce=%s, sig=%s", Scheme, s.id, ts, nonce, base64.RawURLEncoding.EncodeToString(sig)), nil
}

// JoinProof proves, for /v1/join, that the joiner holds this key: it signs
// the origin, the email as sent, and the code's normal form. It returns the
// timestamp it signed and the proof.
func (s *Signer) JoinProof(email, code string) (ts, proof string) {
	ts = strconv.FormatInt(s.Time().Unix(), 10)
	sig := ed25519.Sign(s.key, devicekey.JoinProofBase(s.origin, email, code, ts))
	return ts, base64.RawURLEncoding.EncodeToString(sig)
}

// ClockError is a clock too far from the server's to correct.
type ClockError struct{ Off time.Duration }

func (e *ClockError) Error() string {
	return fmt.Sprintf("this machine's clock is %d minutes off the server's; fix the system clock", int64(e.Off.Round(time.Minute)/time.Minute))
}

// Correct measures the offset from a clock_skew refusal's server_time, to
// retry once. It refuses a correction beyond MaxOffset.
func (s *Signer) Correct(serverTime int64) (time.Duration, error) {
	now := s.Now()
	off := time.Unix(serverTime, 0).Sub(now).Round(time.Second)
	if off > MaxOffset || off < -MaxOffset {
		return 0, &ClockError{Off: off}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset, s.offsetAt = off, now
	return off, nil
}
