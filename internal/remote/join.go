package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
)

// JoinRequest is what a person types to join with an invite (§4).
type JoinRequest struct {
	Email    string
	Code     string
	Label    string
	KeyStore string
}

// Joined is the server's answer to a join.
type Joined struct {
	DeviceID       string            `json:"device_id"`
	Principal      string            `json:"principal"`
	Workspace      JoinedWorkspace   `json:"workspace"`
	Purpose        string            `json:"purpose"`
	Grants         map[string]string `json:"grants"`
	RevokedDevices []string          `json:"revoked_devices"`
	ServerTime     int64             `json:"server_time"`
	// ClockOffset is the correction measured when the first attempt was
	// refused for clock skew, and when; zero otherwise.
	ClockOffset   time.Duration `json:"-"`
	ClockOffsetAt time.Time     `json:"-"`
}

// JoinedWorkspace names the workspace a device joined.
type JoinedWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrJoinUncertain is a join whose answer never came: the device may or may
// not have been added.
var ErrJoinUncertain = errors.New("the server's answer to the join was lost; the device may or may not have been added")

// ErrInvitesUnsupported is a server without device sign-in.
var ErrInvitesUnsupported = errors.New("this server doesn't support invites; sign in with clawbridge login")

// JoinError is a join the server refused: nothing was added.
type JoinError struct {
	Status        int
	Code, Message string
}

func (e *JoinError) Error() string {
	if e.Message != "" {
		return "join refused: " + e.Message
	}
	return fmt.Sprintf("join refused (%d, %s)", e.Status, e.Code)
}

// JoinServer checks that serverURL takes device sign-in at its own origin,
// and returns the origin the join proof and every signature cover.
func JoinServer(ctx context.Context, serverURL string, httpClient *http.Client) (string, error) {
	base, err := validateDiscoveryServerURL(serverURL)
	if err != nil {
		return "", errors.New("the server URL must be https://HOST[:PORT] (http only for a numeric loopback address), with no path")
	}
	u, _ := url.Parse(base)
	origin := Origin(u)
	discovered, err := DiscoverOAuthClient(ctx, base, httpClient)
	if err != nil {
		return "", fmt.Errorf("cannot read the server's sign-in settings: %w", err)
	}
	if discovered.AuthMethods == nil {
		return "", ErrInvitesUnsupported
	}
	if !slices.Contains(discovered.AuthMethods, "device") || discovered.PublicOrigin == "" {
		return "", errors.New("this server doesn't accept device keys; ask its owner, or sign in with clawbridge login")
	}
	if discovered.PublicOrigin != origin {
		return "", fmt.Errorf("this server's public origin is %s; join with --server %s", discovered.PublicOrigin, discovered.PublicOrigin)
	}
	return origin, nil
}

// Join binds key to the invite for in.Email (POST /v1/join, §4), proving it
// holds the key. A clock_skew refusal is corrected once by the server's
// time. The answer must name key's device and the email.
func Join(ctx context.Context, origin string, key ed25519.PrivateKey, in JoinRequest, httpClient *http.Client) (Joined, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	code, ok := devicekey.NormalCode(in.Code)
	if !ok {
		return Joined{}, errors.New("an invite code is CBJ- and 16 letters and digits, as the inviter gave it")
	}
	client := discoveryHTTPClient(httpClient)
	client.Timeout = 30 * time.Second
	signer := devicesign.NewSigner(key, origin, 0, time.Time{})
	var joined Joined
	for attempt := 0; ; attempt++ {
		ts, proof := signer.JoinProof(email, code)
		body, _ := json.Marshal(map[string]string{
			"email": email, "code": code, "public_key": devicekey.EncodePublicKey(key.Public().(ed25519.PublicKey)),
			"label": in.Label, "key_store": in.KeyStore, "ts": ts, "proof": proof,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/v1/join", bytes.NewReader(body))
		if err != nil {
			return Joined{}, errors.New("invalid join request")
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return Joined{}, ctx.Err()
			}
			return Joined{}, ErrJoinUncertain
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if err != nil {
			return Joined{}, ErrJoinUncertain
		}
		if response.StatusCode == http.StatusOK {
			if json.Unmarshal(raw, &joined) != nil {
				return Joined{}, ErrJoinUncertain
			}
			break
		}
		if attempt == 0 && response.StatusCode == http.StatusUnauthorized {
			if serverTime, skewed := clockSkewTime(raw); skewed {
				if _, err = signer.Correct(serverTime); err != nil {
					return Joined{}, err
				}
				continue
			}
		}
		if response.StatusCode >= 500 {
			return Joined{}, ErrJoinUncertain
		}
		var refusal struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &refusal)
		message, _ := bus.SafePublicText(refusal.Error.Message)
		if response.StatusCode == http.StatusNotFound {
			return Joined{}, ErrInvitesUnsupported
		}
		return Joined{}, &JoinError{Status: response.StatusCode, Code: refusal.Error.Code, Message: message}
	}
	if joined.DeviceID != devicekey.ID(key.Public().(ed25519.PublicKey)) || joined.Principal != email || joined.Workspace.ID == "" {
		// The server answered, but not about this key: whether the key was
		// added is unknown, so the caller keeps it (R85-L3).
		return Joined{}, fmt.Errorf("%w: its answer named another device or email, so nothing was configured", ErrJoinUncertain)
	}
	joined.ClockOffset, joined.ClockOffsetAt = signer.Offset()
	return joined, nil
}
