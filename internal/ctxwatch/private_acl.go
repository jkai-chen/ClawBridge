package ctxwatch

import (
	"errors"
	"fmt"
)

// The Windows privacy rules for context records, kept free of Windows calls
// so their tests run on every OS. They are notify's rules (notify's
// private_acl.go), copied: notify imports ctxwatch, so ctxwatch can't import
// them, and ctxwatch's refusals name the record and say what to do.

// Well-known SIDs and ACE fields used by the Windows private-record check.
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

// notPrivateError is why a record (or the context directory) is not private
// to the user, and what to do about it.
type notPrivateError struct{ reason string }

func (e *notPrivateError) Error() string { return "is not private: " + e.reason }

// privateOwner accepts a record owned by this user, or by the Administrators
// group when this process runs elevated as one of its members: Windows makes
// the group, not the user, the default owner of what an elevated
// administrator creates. Anyone who can act as that group can already read
// and take ownership of any file, so it grants no access the user lacks; a
// non-elevated or non-admin process never accepts it, because the group's
// owner then is someone else who could have planted the record.
func privateOwner(owner, user string, elevatedAdministrator bool) error {
	switch {
	case owner != "" && owner == user:
	case owner == sidAdministrators && elevatedAdministrator:
	case owner == sidAdministrators:
		return &notPrivateError{"it was created by an elevated (administrator) session; run elevated, or delete it"}
	default:
		return &notPrivateError{"it is owned by another user; delete it"}
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
			return &notPrivateError{"its ACL has an entry of an unsupported type; delete it"}
		case e.sid != "" && (e.sid == user || e.sid == sidLocalSystem || e.sid == sidAdministrators):
		case e.sid == sidCreatorOwner && e.flags&aceInheritOnly != 0:
		default:
			return &notPrivateError{"its ACL lets others than this user, SYSTEM and Administrators in; delete it"}
		}
	}
	return nil
}

// privacyError is a privacy refusal of path. A Windows refusal names the
// record, as "the queue record <path> is not private: <why>; <what to do>";
// any other (every Unix one) keeps its message, fallback with path and err.
func privacyError(kind, path string, err error, fallback string) error {
	var np *notPrivateError
	if errors.As(err, &np) {
		return fmt.Errorf("the %s %s %s", kind, path, np.Error())
	}
	return fmt.Errorf(fallback, path, err)
}
