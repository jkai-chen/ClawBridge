package ctxwatch

import (
	"errors"
	"strings"
	"testing"
)

// The Windows record rules are pure, so every OS tests them; the Windows
// calls that feed them are tested in private_windows_test.go.
func TestPrivateACLAcceptsUserProfileEntriesOnly(t *testing.T) {
	const user = "S-1-5-21-1000-2000-3000-1001"
	allow := func(sid string) aclEntry { return aclEntry{kind: aceAllowed, sid: sid} }
	for name, c := range map[string]struct {
		entries []aclEntry
		ok      bool
	}{
		"makePrivate DACL":            {[]aclEntry{allow(user)}, true},
		"empty DACL":                  {nil, true},
		"user profile ACL":            {[]aclEntry{allow(sidLocalSystem), allow(sidAdministrators), allow(user)}, true},
		"deny entries narrow access":  {[]aclEntry{{kind: aceDenied, sid: "S-1-1-0"}, allow(user)}, true},
		"inherit-only creator owner":  {[]aclEntry{allow(user), {kind: aceAllowed, flags: aceInheritOnly, sid: sidCreatorOwner}}, true},
		"creator owner on the object": {[]aclEntry{allow(user), allow(sidCreatorOwner)}, false},
		"everyone":                    {[]aclEntry{allow(user), allow("S-1-1-0")}, false},
		"authenticated users":         {[]aclEntry{allow(user), allow("S-1-5-11")}, false},
		"users group":                 {[]aclEntry{allow(user), allow("S-1-5-32-545")}, false},
		"another user":                {[]aclEntry{allow("S-1-5-21-1000-2000-3000-1002")}, false},
		"inherit-only other user":     {[]aclEntry{allow(user), {kind: aceAllowed, flags: aceInheritOnly, sid: "S-1-5-21-1000-2000-3000-1002"}}, false},
		"unsupported entry type":      {[]aclEntry{allow(user), {kind: 5, sid: user}}, false},
		"unreadable SID":              {[]aclEntry{allow("")}, false},
	} {
		if err := privateACL(c.entries, user); (err == nil) != c.ok {
			t.Errorf("%s: accepted=%v, want %v (%v)", name, err == nil, c.ok, err)
		}
	}
}

func TestPrivateOwnerAcceptsAdministratorsOnlyWhenElevatedAsOne(t *testing.T) {
	const user = "S-1-5-21-1000-2000-3000-1001"
	for name, c := range map[string]struct {
		owner    string
		elevated bool
		ok       bool
	}{
		"this user":                       {user, false, true},
		"administrators, elevated member": {sidAdministrators, true, true},
		"administrators, not elevated":    {sidAdministrators, false, false},
		"another user, elevated":          {"S-1-5-21-1000-2000-3000-1002", true, false},
		"system":                          {sidLocalSystem, true, false},
		"unreadable owner":                {"", true, false},
	} {
		if err := privateOwner(c.owner, user, c.elevated); (err == nil) != c.ok {
			t.Errorf("%s: accepted=%v, want %v", name, err == nil, c.ok)
		}
	}
}

// A Windows refusal names the record and says what to do; any other error
// (every Unix one) keeps its message.
func TestPrivacyErrorNamesTheRecord(t *testing.T) {
	const user = "S-1-5-21-1000-2000-3000-1001"
	const path = `C:\state\context\agent.queue.json`
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"another owner": {privateOwner("S-1-5-21-1000-2000-3000-1002", user, false),
			`the queue record C:\state\context\agent.queue.json is not private: it is owned by another user; delete it`},
		"elevated owner": {privateOwner(sidAdministrators, user, false),
			`the queue record C:\state\context\agent.queue.json is not private: it was created by an elevated (administrator) session; run elevated, or delete it`},
		"broad ACL": {privateACL([]aclEntry{{kind: aceAllowed, sid: user}, {kind: aceAllowed, sid: "S-1-1-0"}}, user),
			`the queue record C:\state\context\agent.queue.json is not private: its ACL lets others than this user, SYSTEM and Administrators in; delete it`},
		"unix": {errors.New("is readable by other users; it must be 0600"),
			`C:\state\context\agent.queue.json is readable by other users; it must be 0600`},
	} {
		if got := privacyError("queue record", path, c.err, "%s %s"); got == nil || got.Error() != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if got := privacyError("binding record", "/b.json", errors.New("is not owned by this user"), "the binding record %s %s"); !strings.HasPrefix(got.Error(), "the binding record /b.json is not owned") {
		t.Error(got)
	}
}
