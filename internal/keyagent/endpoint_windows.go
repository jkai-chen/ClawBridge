//go:build windows

package keyagent

import "golang.org/x/sys/windows"

// userSID is this process's user SID in string form.
func userSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// endpointFor is the pipe name for a canonical state directory; there is no
// directory to make or check.
func endpointFor(state string, _ bool) (string, error) {
	sid, err := userSID()
	if err != nil {
		return "", err
	}
	return pipeName(sid, stateHash(state)), nil
}
