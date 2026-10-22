package notify

import "errors"

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
