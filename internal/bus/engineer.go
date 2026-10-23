package bus

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// engineerRunes caps the engineer's name in an agent marker.
const engineerRunes = 40

// EngineerName is the engineer an agent marker names: the session owner's
// Linear display name when one is recorded, else the owner's verified
// principal (an email on a hosted server). The display name loses every
// character that could mention, link or format in Linear; the result is
// cut to engineerRunes characters with "…".
func EngineerName(display, principal string) string {
	if name := cleanDisplayName(display); name != "" {
		return name
	}
	return capRunes(strings.TrimSpace(principal))
}

// ValidEngineer reports whether s could be an EngineerName result: short,
// one line, and free of Linear markup.
func ValidEngineer(s string) bool {
	return s != "" && utf8.ValidString(s) && capRunes(s) == s && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || strings.ContainsRune("*_[]()<>#`~|!\\{}·", r)
	})
}

func cleanDisplayName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsControl(r) || strings.ContainsRune("@*_[]()<>#`~|!\\{}:/·", r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return capRunes(strings.Join(strings.Fields(b.String()), " "))
}

func capRunes(s string) string {
	if utf8.RuneCountInString(s) <= engineerRunes {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:engineerRunes-1])) + "…"
}

// engineerOf names the engineer behind a session: its owner, as recorded by
// the authority. The caller never supplies it.
func (s *Store) engineerOf(ctx context.Context, tx *transaction, wid string, actor map[string]any) (string, error) {
	owner, _ := actor["owner_principal"].(string)
	if !s.postgres {
		return EngineerName("", owner), nil
	}
	display := ""
	err := tx.QueryRowContext(ctx, `SELECT display_name FROM members WHERE workspace_id=? AND principal=?`, wid, owner).Scan(&display)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return EngineerName(display, owner), nil
}

// setDisplayName records the caller's own Linear display name in the
// workspace: the member override, and what `linear connect` captures
// (if_unset: 1, which keeps a name already recorded). An empty name clears
// it, so the marker falls back to the principal.
func (s *Store) setDisplayName(ctx context.Context, tx *transaction, wid string, a map[string]any) (any, error) {
	if !s.postgres {
		return nil, invalid("a display name is recorded on a hosted server; a local store names the OS user")
	}
	raw := str(a, "display_name")
	if utf8.RuneCountInString(raw) > 200 {
		return nil, invalid("a display name is up to 200 characters")
	}
	current := ""
	err := tx.QueryRowContext(ctx, `SELECT display_name FROM members WHERE workspace_id=? AND principal=?`, wid, s.principal).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("workspace membership required")
	}
	if err != nil {
		return nil, err
	}
	if n, _ := integer(a["if_unset"]); n == 1 && current != "" {
		return map[string]any{"display_name": current, "engineer": EngineerName(current, s.principal), "kept": true}, nil
	}
	name := cleanDisplayName(raw)
	if _, err = tx.ExecContext(ctx, `UPDATE members SET display_name=? WHERE workspace_id=? AND principal=?`, name, wid, s.principal); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, "", "", "member", s.principal, "display_name_set", map[string]any{"set": name != ""}); err != nil {
		return nil, err
	}
	return map[string]any{"display_name": name, "engineer": EngineerName(name, s.principal), "kept": false}, nil
}
