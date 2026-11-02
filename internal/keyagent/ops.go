package keyagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/sealed"
)

// wrongPassDelay is the pause after a wrong passphrase to rotate, the same
// second the CLI waits; tests shorten it.
var wrongPassDelay = time.Second

// MaxTimestampSkew is how far from the agent's clock, corrected by
// Options.ClockOffset, a timestamp to sign may be.
const MaxTimestampSkew = 15 * time.Minute

// nonceBytes is the size of a request nonce, as devicesign makes it.
const nonceBytes = 16

// handler runs the operations against the store. One mutex serialises them,
// so a rotation is never half seen by another request.
type handler struct {
	mu    sync.Mutex
	store *sealed.Store
	kid   string // first 8 hex digits of the current key ID

	idle    time.Duration
	offset  time.Duration // the profile's clock correction: server time minus local
	origin  string        // the only origin sign serves; "" is any
	now     func() time.Time
	lastReq time.Time // under mu
	stop    func()    // asks Serve to exit
}

// touch records a request as the idle clock's last.
func (h *handler) touch() { h.lastReq = h.now() }

// idleLeft is the seconds before the idle timeout fires, or -1 when it is off.
func (h *handler) idleLeft() int64 {
	if h.idle <= 0 {
		return -1
	}
	left := h.idle - h.now().Sub(h.lastReq)
	if left < 0 {
		left = 0
	}
	return int64(left / time.Second)
}

// idleExpired reports whether the idle timeout has passed.
func (h *handler) idleExpired() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.idle > 0 && h.now().Sub(h.lastReq) >= h.idle
}

// do runs one request and returns its response. It returns only fixed
// errors.
func (h *handler) do(req *request) *response {
	h.mu.Lock()
	resp, delay := h.dispatch(req)
	h.mu.Unlock()
	// The pause after a wrong passphrase is taken with the lock released, so
	// a loop of wrong rotations does not block sign or unseal.
	if delay > 0 {
		time.Sleep(delay)
	}
	return resp
}

// dispatch runs a request under h.mu. delay is a pause the caller takes
// after releasing the lock.
func (h *handler) dispatch(req *request) (resp *response, delay time.Duration) {
	// A status request does not count as use: a UI polling the lock state
	// must not keep an idle agent alive.
	if req.Op != OpStatus {
		h.touch()
	}
	ok := &response{V: Version, OK: true}
	var err error
	switch req.Op {
	case OpStatus:
		locked := false
		left := h.idleLeft()
		ok.Locked, ok.KidPrefix, ok.Mode, ok.IdleLeftS = &locked, h.kid, ModePassphrase, &left
	case OpDeviceNew:
		err = h.deviceNew(ok)
	case OpSign:
		err = h.sign(req, ok)
	case OpUnseal:
		err = h.unseal(req, ok)
	case OpSeal:
		err = h.seal(req)
	case OpRotate:
		err = h.rotate(req)
		if sealed.CodeOf(err) == sealed.CodeWrongPassphrase {
			delay = wrongPassDelay
		}
	case OpLock:
		h.stop()
	default:
		err = newError(CodeUnknownOp)
	}
	if err != nil {
		return failure(err), delay
	}
	return ok, delay
}

// failure is the response for err: a fixed code and message.
func failure(err error) *response {
	var e *Error
	if !errors.As(err, &e) {
		e = fromSealed(err)
	}
	return &response{V: Version, Code: e.Code, Msg: e.Msg}
}

// fromSealed maps a sealed-store failure to a fixed error. A store that has
// dropped its key (after Lock) is locked.
func fromSealed(err error) *Error {
	switch c := sealed.CodeOf(err); c {
	case sealed.CodeNotFound:
		return newError(CodeNotFound)
	case sealed.CodeInvalid:
		return newError(CodeInvalid)
	case sealed.CodeWrongPassphrase:
		return newError(CodeWrongPass)
	case sealed.CodePassphraseMode:
		return newError(CodeLocked)
	case "":
		return newError(CodeInternal)
	default:
		return &Error{Code: string(c), Msg: "the sealed key storage failed (" + string(c) + ")"}
	}
}

// secretKind reports whether kind is one the agent will unseal or seal.
func secretKind(kind string) bool {
	return kind == sealed.KindLinear || kind == sealed.KindLinearAPI
}

func (h *handler) unseal(req *request, out *response) error {
	if !secretKind(req.Kind) {
		return newError(CodeForbidden)
	}
	if req.Kind == sealed.KindLinearAPI {
		if req.Purpose != PurposeLinearKey && req.Purpose != PurposeLinearView {
			return newError(CodePurposeRequired)
		}
	} else if req.Purpose != "" {
		return newError(CodeBadRequest)
	}
	pt, err := h.store.Unseal(req.Kind, req.ID)
	if err != nil {
		return err
	}
	out.Plaintext = pt
	return nil
}

func (h *handler) seal(req *request) error {
	if !secretKind(req.Kind) {
		return newError(CodeForbidden)
	}
	if len(req.Plaintext) == 0 {
		return newError(CodeInvalid)
	}
	return h.store.Seal(req.Kind, req.ID, req.Plaintext)
}

func (h *handler) rotate(req *request) error {
	if len(req.Passphrase) == 0 {
		return newError(CodeInvalid)
	}
	err := h.store.RotateWrapped(req.Passphrase)
	if err != nil {
		return err
	}
	if rep := h.store.Check(); len(rep.DataKey.KidPrefix) == 8 {
		h.kid = rep.DataKey.KidPrefix
	}
	return nil
}

// deviceNew makes a device key inside the agent, seals its seed as the
// device record and returns only the public key. The ID derives from the
// key, so the agent names the device; no record can already exist under it
// except by a 2^-130 accident, which is still refused.
func (h *handler) deviceNew(out *response) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return newError(CodeInternal)
	}
	seed := priv.Seed()
	defer func() { zero(seed); zero(priv) }()
	id := devicekey.ID(pub)
	existing, err := h.store.List(sealed.KindDevice)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e == id {
			return newError(CodeExists)
		}
	}
	if err = h.store.Seal(sealed.KindDevice, id, seed); err != nil {
		return err
	}
	out.DeviceID = id
	out.PublicKey = devicekey.EncodePublicKey(pub)
	return nil
}

// sign signs one request for a device. The seed is unsealed for this call
// only and zeroed after it; it is never cached and never returned. The
// signed bytes are devicekey.RequestBaseSum of validated fields, never
// anything the caller composed.
func (h *handler) sign(req *request, out *response) error {
	origin := strings.ToLower(req.Origin)
	if !validMethod(req.Method) || !validOrigin(origin) || !isDigits(req.Timestamp, 12) ||
		!isLowerHex(req.BodySHA256, sha256.Size*2) || !devicekey.ValidID(req.DeviceID) ||
		!printable(req.Path, 4096) || req.Path[0] != '/' {
		return newError(CodeInvalid)
	}
	// A server origin the profile fixed is the only one served, and the
	// origin is signed in its lower-case form, as remote.Origin writes it.
	if h.origin != "" && origin != h.origin {
		return newError(CodeOriginNotAllowed)
	}
	// A signature is made only for a time near now (on the server's clock),
	// so a program that is allowed to ask while the agent is unlocked cannot
	// collect signatures dated for use after the person has locked it.
	ts, _ := strconv.ParseInt(req.Timestamp, 10, 64)
	if d := time.Unix(ts, 0).Sub(h.now().Add(h.offset)); d > MaxTimestampSkew || d < -MaxTimestampSkew {
		return newError(CodeTimestampRange)
	}
	var sum [sha256.Size]byte
	if _, err := hex.Decode(sum[:], []byte(req.BodySHA256)); err != nil {
		return newError(CodeInvalid)
	}
	seed, err := h.store.Unseal(sealed.KindDevice, req.DeviceID)
	if err != nil {
		return err
	}
	defer zero(seed)
	if len(seed) != ed25519.SeedSize {
		return newError(CodeInternal)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	defer zero(priv)
	if devicekey.ID(priv.Public().(ed25519.PublicKey)) != req.DeviceID {
		return newError(CodeInternal)
	}
	var n [nonceBytes]byte
	if _, err = rand.Read(n[:]); err != nil {
		return newError(CodeInternal)
	}
	nonce := base64.RawURLEncoding.EncodeToString(n[:])
	sig := ed25519.Sign(priv, devicekey.RequestBaseSum(req.Method, origin, req.Path, req.Timestamp, nonce, sum))
	out.Signature = base64.RawURLEncoding.EncodeToString(sig)
	out.Nonce = nonce
	return nil
}
