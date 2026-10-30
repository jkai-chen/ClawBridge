package bus

import (
	"context"
	"strings"
)

// IssueHistoryAppUnavailable refuses an issue history read where no
// ClawBridge Linear app answers it: a local store, or a hosted server
// without the app (FR21). Linear's MCP tools read no history, so there is
// no fallback.
const IssueHistoryAppUnavailable = "app_unavailable: issue history needs the ClawBridge Linear app on the server"

// maxLinearUserNames bounds one lookup: a history page names at most two
// assignees and an actor per entry.
const maxLinearUserNames = 200

// LinearUserNames are the names ClawBridge stores for Linear users of a
// project, by lower-case Linear user ID: the display name of the member
// the project's team roles, else its trusted intake list, link the user
// to. A user linked to no member, or to one with no name, is left out;
// a name with an "@" is never returned. Only a hosted store has names.
func (s *Store) LinearUserNames(ctx context.Context, pid string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	want := map[string]bool{}
	for _, id := range ids {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" && len(want) < maxLinearUserNames {
			want[id] = true
		}
	}
	if len(want) == 0 || !s.postgres {
		return out, nil
	}
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		p, err := one(ctx, tx, `SELECT workspace_id FROM projects WHERE id=?`, pid)
		if err != nil {
			return err
		}
		wid := str(p, "workspace_id")
		principals := map[string]string{}
		m, _, err := readTeamRoles(ctx, tx, pid)
		if err != nil {
			return err
		}
		for _, person := range m.People {
			if id := strings.ToLower(person.LinearUserID); want[id] && person.Principal != "" && principals[id] == "" {
				principals[id] = person.Principal
			}
		}
		trusted, err := query(ctx, tx, `SELECT linear_user_id,principal FROM intake_trusted WHERE project_id=? AND principal IS NOT NULL ORDER BY added_at,linear_user_id`, pid)
		if err != nil {
			return err
		}
		for _, row := range trusted {
			if id := strings.ToLower(str(row, "linear_user_id")); want[id] && principals[id] == "" && str(row, "principal") != "" {
				principals[id] = str(row, "principal")
			}
		}
		for id, principal := range principals {
			rows, err := query(ctx, tx, `SELECT display_name FROM members WHERE workspace_id=? AND principal=?`, wid, principal)
			if err != nil {
				return err
			}
			if len(rows) == 1 {
				if name := strings.TrimSpace(str(rows[0], "display_name")); name != "" && !strings.Contains(name, "@") {
					out[id] = name
				}
			}
		}
		return nil
	})
	return out, err
}
