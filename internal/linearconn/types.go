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
}

type Error struct{ Kind string }

func (e *Error) Error() string  { return "Linear connection: " + e.Kind }
func (e *Error) Code() string   { return e.Kind }
func problem(kind string) error { return &Error{Kind: kind} }
