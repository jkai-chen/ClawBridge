// Package linearconn manages personal native Linear credentials. It never reads
// repository configuration and does not authorize implicitly.
package linearconn

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/zalando/go-keyring"
)

const Service = "clawbridge.linear.v1"
const Origin = "https://mcp.linear.app"

type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

func Endpoint(access Access) string {
	if access == Read {
		return Origin + "/mcp/readonly"
	}
	return Origin + "/mcp"
}

type Profile struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Access         Access `json:"access"`
	AccountLabel   string `json:"account_label,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	WorkspaceLabel string `json:"workspace_label,omitempty"`
	// KeyStore is where the connection's credential is kept, read from the
	// profile's store record beside its file (empty: the OS key store). It
	// is never in profiles.json, which keeps main's schema, so an older
	// binary still reads the index.
	KeyStore string `json:"-"`
}

// Store names the store the profile's credential is kept in.
func (p Profile) Store() string {
	if p.KeyStore == StoreFile {
		return StoreFile
	}
	return StoreKeyring
}

// Identity contains only provider-verified, safe display metadata. An unavailable
// workspace UUID must remain empty, rather than being inferred from a label.
type Identity struct {
	AccountLabel   string
	WorkspaceID    string
	WorkspaceLabel string
}

type SecureStore interface {
	Get(service, account string) (string, error)
	Set(service, account, value string) error
	Delete(service, account string) error
}

type NativeStore struct{}

func (NativeStore) Get(s, a string) (string, error) { return keyring.Get(s, a) }
func (NativeStore) Set(s, a, v string) error        { return keyring.Set(s, a, v) }
func (NativeStore) Delete(s, a string) error        { return keyring.Delete(s, a) }

type Config struct {
	Directory  string
	Store      SecureStore
	HTTPClient *http.Client
	Verify     func(context.Context, *http.Client, Access) (Identity, error)
	Now        func() time.Time
	// KeyringCheck, when set, says before an authorization whether the OS
	// key store can take a new credential, so a missing one is named
	// before the browser opens rather than after.
	KeyringCheck func(context.Context) error
}

type ConnectOptions struct {
	Name      string
	ProfileID string
	Access    Access
	NoBrowser bool
	// PresentURL presents an authorization URL to the operator. For browser mode
	// the caller opens it explicitly; no browser is ever started implicitly here.
	PresentURL func(string) error
	// FetchCode and RedirectURL are paired injection seams for synthetic tests.
	// Ordinary callers leave both unset to use the bounded loopback listener.
	FetchCode   auth.AuthorizationCodeFetcher
	RedirectURL string
	// KeyStore is where the credential goes: StoreKeyring or StoreFile.
	// Empty keeps a reconnected profile's store, and is the keyring for a
	// new one.
	KeyStore string
}

// Error is a Linear connection failure. Kind is a stable code; Detail, when
// set, adds the specifics a person needs, such as who holds a lock.
type Error struct{ Kind, Detail string }

func (e *Error) Error() string {
	if e.Detail != "" {
		return "Linear connection: " + e.Kind + ": " + e.Detail
	}
	return "Linear connection: " + e.Kind
}
func (e *Error) Code() string   { return e.Kind }
func problem(kind string) error { return &Error{Kind: kind} }
