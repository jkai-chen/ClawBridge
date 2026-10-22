package bus

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// transaction keeps SQL dialect selection attached to one transaction, never to
// mutable request identity or a caller-controlled context value.
type transaction struct {
	*sql.Tx
	postgres bool
	// pure is set when the request is a pure read, so the transaction was
	// opened read-only and takes neither advisory lock.
	pure    bool
	actorID string
}

func (t *transaction) bind(q string) string {
	if !t.postgres {
		return q
	}
	q = strings.ReplaceAll(q, "rowid AS sequence,*", "*")
	q = strings.ReplaceAll(q, "rowid", "sequence")
	var out strings.Builder
	n := 0
	quoted := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			out.WriteByte(c)
			if quoted && i+1 < len(q) && q[i+1] == '\'' {
				i++
				out.WriteByte('\'')
				continue
			}
			quoted = !quoted
			continue
		}
		if c == '?' && !quoted {
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		} else {
			out.WriteByte(c)
		}
	}
	return out.String()
}
func (t *transaction) ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.bind(q), a...)
}
func (t *transaction) QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, t.bind(q), a...)
}
func (t *transaction) QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.bind(q), a...)
}
