package keyagent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"time"

	"clawbridge/internal/devicekey"
)

// callTimeout bounds a whole call, which may include a rotation. It is a
// variable so tests can shorten it.
var callTimeout = 60 * time.Second

// Client talks to the agent for one state directory. It holds no key and no
// connection: each call is one connection and one request.
type Client struct {
	state string
	// dial is the transport; tests replace it.
	dial func(state string) (conn, error)
}

// Dial returns a client for the agent serving stateDir. It asks the agent
// for its status, so an absent or locked agent is ErrLocked here.
func Dial(stateDir string) (*Client, error) {
	state, err := canonicalState(stateDir)
	if err != nil {
		return nil, err
	}
	c := &Client{state: state, dial: dial}
	if _, err = c.Status(); err != nil {
		return nil, err
	}
	return c, nil
}

// call sends req and returns the agent's response. A failure response is an
// *Error. req's secret fields are zeroed once sent.
func (c *Client) call(req *request) (*response, error) {
	req.V = Version
	defer req.wipe()
	conn, err := c.dial(c.state)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(callTimeout))
	b, err := json.Marshal(req)
	if err != nil {
		return nil, errors.New("cannot encode the request")
	}
	if len(b) > MaxFrame {
		zero(b)
		return nil, newError(CodeTooLarge)
	}
	err = writeFrame(conn, b)
	zero(b)
	if err != nil {
		return nil, errors.New("cannot send the request to the key agent")
	}
	raw, err := readFrame(io.LimitReader(conn, MaxFrame+4))
	if err != nil {
		// The agent exited, or was locked, before it answered.
		return nil, errors.New("the key agent did not answer")
	}
	defer zero(raw)
	dec := json.NewDecoder(bytes.NewReader(raw))
	var resp response
	if err = dec.Decode(&resp); err != nil || resp.V != Version {
		return nil, errors.New("the key agent's answer is malformed")
	}
	if !resp.OK {
		zero(resp.Plaintext)
		if _, known := messages[resp.Code]; known {
			return nil, newError(resp.Code)
		}
		return nil, &Error{Code: resp.Code, Msg: resp.Msg}
	}
	return &resp, nil
}

// Status is the agent's state; it never carries key material.
type Status struct {
	Locked    bool
	KidPrefix string
	Mode      string
	// IdleLeft is the time before the idle timeout; negative when it is off.
	IdleLeft time.Duration
}

// Status asks the agent for its state.
func (c *Client) Status() (Status, error) {
	r, err := c.call(&request{Op: OpStatus})
	if err != nil {
		return Status{}, err
	}
	st := Status{KidPrefix: r.KidPrefix, Mode: r.Mode, IdleLeft: -1}
	if r.Locked != nil {
		st.Locked = *r.Locked
	}
	if r.IdleLeftS != nil && *r.IdleLeftS >= 0 {
		st.IdleLeft = time.Duration(*r.IdleLeftS) * time.Second
	}
	return st, nil
}

// DeviceNew makes a device key inside the agent, seals its seed as the
// device record and returns the device's ID and public key. The seed never
// leaves the agent. The ID derives from the key, so the agent chooses it.
func (c *Client) DeviceNew() (id string, pub ed25519.PublicKey, err error) {
	r, err := c.call(&request{Op: OpDeviceNew})
	if err != nil {
		return "", nil, err
	}
	pub, err = devicekey.ParsePublicKey(r.PublicKey)
	if err != nil || devicekey.ID(pub) != r.DeviceID {
		return "", nil, errors.New("the key agent's answer is malformed")
	}
	return r.DeviceID, pub, nil
}

// SignRequest is the structured request to sign. The agent builds the
// canonical string from these fields and no others.
type SignRequest struct {
	DeviceID string
	Method   string
	// Origin is the server's public origin, scheme://host[:port].
	Origin string
	// Path is the request target as sent, path and query.
	Path string
	// BodySHA256 is the lower-case hex SHA-256 of the request body.
	BodySHA256 string
	// Timestamp is the Unix time in seconds, as written in the header.
	Timestamp string
}

// Signed is a signature and the nonce the agent put in the signed string;
// both go in the Authorization header.
type Signed struct {
	// Signature is unpadded base64url, as devicesign writes it.
	Signature string
	Nonce     string
}

// Sign has the agent sign one request.
func (c *Client) Sign(req SignRequest) (Signed, error) {
	r, err := c.call(&request{Op: OpSign, DeviceID: req.DeviceID, Method: req.Method, Origin: req.Origin,
		Path: req.Path, BodySHA256: req.BodySHA256, Timestamp: req.Timestamp})
	if err != nil {
		return Signed{}, err
	}
	return Signed{Signature: r.Signature, Nonce: r.Nonce}, nil
}

// Unseal returns the plaintext of a linear or linear-api record. purpose is
// required for linear-api (PurposeLinearKey or PurposeLinearView).
func (c *Client) Unseal(kind, id, purpose string) ([]byte, error) {
	r, err := c.call(&request{Op: OpUnseal, Kind: kind, ID: id, Purpose: purpose})
	if err != nil {
		return nil, err
	}
	return r.Plaintext, nil
}

// Seal stores plaintext as a linear or linear-api record. The caller keeps
// and zeroes its own copy.
func (c *Client) Seal(kind, id string, plaintext []byte) error {
	// call zeroes the request's secret fields; send a copy.
	_, err := c.call(&request{Op: OpSeal, Kind: kind, ID: id, Plaintext: append([]byte(nil), plaintext...)})
	return err
}

// Rotate replaces the data key, wrapped under the same passphrase. The
// caller keeps and zeroes its own copy.
func (c *Client) Rotate(pass []byte) error {
	_, err := c.call(&request{Op: OpRotate, Passphrase: append([]byte(nil), pass...)})
	return err
}

// Lock makes the agent zero its key and exit.
func (c *Client) Lock() error {
	_, err := c.call(&request{Op: OpLock})
	return err
}
