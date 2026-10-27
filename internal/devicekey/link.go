package devicekey

import (
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// An invite link carries everything a join needs in one paste
// (live/AUTH-INVITE-DESIGN.md §17.1):
//
//	https://cb.example.com/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB
//
// The code is in the fragment, which a browser never sends, so opening the
// link by mistake shows the server nothing. The email is base64url, so a
// chat client can't mangle its @. Fields are split from the right: the
// workspace name may hold dots, the email and code can't.

// LinkVersion starts every link's fragment.
const LinkVersion = "cbj1"

// LinkPath is the path an invite link names on its server.
const LinkPath = "/join"

// Link is an invite link's parts.
type Link struct {
	// Origin is the server, scheme and host only.
	Origin    string
	Workspace string
	Email     string
	// Code is the invite code as NewCode prints it.
	Code string
}

var linkWorkspace = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ErrNotALink is ParseLink's answer to text that isn't an invite link.
var ErrNotALink = errors.New("not a ClawBridge invite link (https://SERVER/join#cbj1.…)")

// FormatLink makes the link for an invite.
func FormatLink(origin, workspace, email, code string) (string, error) {
	o, err := linkOrigin(origin)
	if err != nil {
		return "", err
	}
	normal, ok := NormalCode(code)
	if !ok || !linkWorkspace.MatchString(workspace) || !strings.Contains(email, "@") {
		return "", errors.New("an invite link needs a workspace name, an email and an invite code")
	}
	return o + LinkPath + "#" + LinkVersion + "." + workspace + "." + base64.RawURLEncoding.EncodeToString([]byte(email)) + "." + CodePrefix + normal[0:4] + "-" + normal[4:8] + "-" + normal[8:12] + "-" + normal[12:16], nil
}

// ParseLink reads a pasted invite link. Surrounding space is ignored; the
// origin must be https, or http on a loopback host.
func ParseLink(s string) (Link, error) {
	s = strings.TrimSpace(s)
	hash := strings.IndexByte(s, '#')
	if hash < 0 {
		return Link{}, ErrNotALink
	}
	u, err := url.Parse(s[:hash])
	if err != nil || u.Path != LinkPath || u.RawQuery != "" || u.User != nil {
		return Link{}, ErrNotALink
	}
	origin, err := linkOrigin(u.Scheme + "://" + u.Host)
	if err != nil {
		return Link{}, err
	}
	fragment := s[hash+1:]
	if !strings.HasPrefix(fragment, LinkVersion+".") {
		if strings.HasPrefix(fragment, "cbj") {
			return Link{}, errors.New("this invite link is from a newer ClawBridge; update clawbridge")
		}
		return Link{}, ErrNotALink
	}
	rest := strings.TrimPrefix(fragment, LinkVersion+".")
	last := strings.LastIndexByte(rest, '.')
	if last < 0 {
		return Link{}, ErrNotALink
	}
	code := rest[last+1:]
	rest = rest[:last]
	mid := strings.LastIndexByte(rest, '.')
	if mid < 0 {
		return Link{}, ErrNotALink
	}
	workspace, rawEmail := rest[:mid], rest[mid+1:]
	email, err := base64.RawURLEncoding.DecodeString(rawEmail)
	normal, ok := NormalCode(code)
	if err != nil || !ok || !linkWorkspace.MatchString(workspace) || !strings.Contains(string(email), "@") || strings.ContainsAny(string(email), " \t\r\n") {
		return Link{}, ErrNotALink
	}
	return Link{Origin: origin, Workspace: workspace, Email: strings.ToLower(string(email)), Code: CodePrefix + normal}, nil
}

// linkOrigin is scheme and host, lowercased: https, or http on loopback.
func linkOrigin(origin string) (string, error) {
	u, err := url.Parse(strings.TrimRight(origin, "/"))
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("an invite link's server must be https://HOST")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	// As the client's own server check: http only for a numeric loopback
	// address.
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", errors.New("an invite link's server must use https (http only on a loopback address)")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}
