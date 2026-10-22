package notify

import (
	"errors"
	"fmt"
)

// Well-known SIDs and ACE fields used by the Windows private-state check.
const (
	sidLocalSystem    = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"
	sidCreatorOwner   = "S-1-3-0"
	aceAllowed        = 0    // ACCESS_ALLOWED_ACE_TYPE
	aceDenied         = 1    // ACCESS_DENIED_ACE_TYPE
	aceInheritOnly    = 0x08 // INHERIT_ONLY_ACE
)

type aclEntry struct {
	kind, flags uint8
	sid         string
}

// privateOwner accepts state owned by this user, or by the Administrators
// group when this process runs elevated as one of its members: Windows makes
// the group, not the user, the default owner of what an elevated
// administrator creates. Anyone who can act as that group can already read
// and take ownership of any file, so it grants no access the user lacks; a
// non-elevated or non-admin process never accepts it, because the group's
// owner then is someone else who could have planted the state.
func privateOwner(path, owner, user string, elevatedAdministrator bool) error {
	switch {
	case owner != "" && owner == user:
	case owner == sidAdministrators && elevatedAdministrator:
	case owner == sidAdministrators:
		return fmt.Errorf("notification state was created by an elevated (administrator) session; delete %s or run elevated", path)
	default:
		return errors.New("notification state owner mismatch")
	}
	return nil
}

// privateACL accepts the protected DACL makePrivate writes and the standard
// Windows user-profile ACL: allow entries only for this user, LocalSystem and
// the Administrators group, which can read any file anyway, plus CREATOR OWNER
// only as an inherit-only entry. Deny entries only narrow access. Any other
// entry, including an allow for any other principal, is refused.
func privateACL(entries []aclEntry, user string) error {
	for _, e := range entries {
		switch {
		case e.kind == aceDenied:
		case e.kind != aceAllowed:
			return errors.New("notification state ACL has an unsupported entry")
		case e.sid != "" && (e.sid == user || e.sid == sidLocalSystem || e.sid == sidAdministrators):
		case e.sid == sidCreatorOwner && e.flags&aceInheritOnly != 0:
		default:
			return errors.New("notification state ACL must allow only this user, SYSTEM and Administrators")
		}
	}
	return nil
}
