package notify

import (
	"strings"
	"testing"
)

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
		if err := privateOwner(`C:\state`, c.owner, user, c.elevated); (err == nil) != c.ok {
			t.Errorf("%s: accepted=%v, want %v", name, err == nil, c.ok)
		}
	}
	// A non-elevated session says why and what to do; others keep the
	// generic refusal.
	err := privateOwner(`C:\state`, sidAdministrators, user, false)
	if err == nil || !strings.Contains(err.Error(), "created by an elevated (administrator) session; delete C:\\state or run elevated") {
		t.Fatal("non-elevated refusal does not explain itself", err)
	}
	if err = privateOwner(`C:\state`, "S-1-5-21-1000-2000-3000-1002", user, false); err == nil || strings.Contains(err.Error(), "elevated") {
		t.Fatal("foreign owner given the elevated-session hint", err)
	}
}
