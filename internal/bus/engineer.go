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
// recorded display name, without every character that could mention, link
// or format in Linear, cut to engineerRunes characters with "…". Without a
// recorded name it is "" and markers name no one: an email would reach
// everyone who can read the issue, guests and outside reviewers included.
func EngineerName(display string) string {
	return cleanDisplayName(display)
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
// the authority. The caller never supplies it. A local store has no
// verified principal (only the OS user), so it names no one.
func (s *Store) engineerOf(ctx context.Context, tx *transaction, wid string, actor map[string]any) (string, error) {
	owner, _ := actor["owner_principal"].(string)
	if !s.postgres {
		return "", nil
	}
	display := ""
	err := tx.QueryRowContext(ctx, `SELECT display_name FROM members WHERE workspace_id=? AND principal=?`, wid, owner).Scan(&display)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return EngineerName(display), nil
}

// setDisplayName records the caller's own Linear display name in the
// workspace: the member override, and what `linear connect` captures
// (if_unset: 1, which keeps a name already recorded). An empty name clears
// it, and markers then name no engineer.
func (s *Store) setDisplayName(ctx context.Context, tx *transaction, wid string, a map[string]any) (any, error) {
	if !s.postgres {
		return nil, invalid("a display name is recorded on a hosted server; a local store names no engineer")
	}
	raw := str(a, "display_name")
	if utf8.RuneCountInString(raw) > 200 {
		return nil, invalid("a display name is up to 200 characters")
	}
	if strings.Contains(raw, "@") {
		// Stripping the "@" would still show an email, or mention a handle.
		return nil, invalid("a display name cannot contain @: markers never show an email or a handle")
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
		return map[string]any{"display_name": current, "engineer": EngineerName(current), "kept": true}, nil
	}
	name := cleanDisplayName(raw)
	if _, err = tx.ExecContext(ctx, `UPDATE members SET display_name=? WHERE workspace_id=? AND principal=?`, name, wid, s.principal); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, "", "", "member", s.principal, "display_name_set", map[string]any{"set": name != ""}); err != nil {
		return nil, err
	}
	return map[string]any{"display_name": name, "engineer": EngineerName(name), "kept": false}, nil
}
