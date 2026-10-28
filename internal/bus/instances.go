package bus

import (
	"context"
	"strings"
)

// ServerInstancePrefix starts the application_name each serving process
// gives its Postgres connections (OpenPostgresServer).
const ServerInstancePrefix = "clawbridge-server-"

// ServerInstances is how many ClawBridge servers share this database, as
// pg_stat_activity shows it. Known is false when it can't be told: this
// server's own name isn't seen (an application_name set in the DSN, a
// pooler in front, or rows the role can't read), or a client of this role
// hides or lacks its name. Never a guessed 1. Pooler is a connection named
// like PgBouncer, which makes the count unreliable.
type ServerInstances struct {
	Live   int  `json:"live"`
	Known  bool `json:"known"`
	Pooler bool `json:"pooler,omitempty"`
}

// InstanceName is the serving store's application_name, "" for any other
// store: serve's other connections (the stream hub's listener) carry it too.
func (s *Store) InstanceName() string { return s.instance }

// LiveServerInstances counts the servers with a connection open to this
// database now. Each server keeps idle connections open, so a quiet one
// still counts.
func (s *Store) LiveServerInstances(ctx context.Context) (ServerInstances, error) {
	if !s.postgres || s.instance == "" {
		return ServerInstances{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(application_name,'') FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' AND usename=current_user`)
	if err != nil {
		return ServerInstances{}, nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return ServerInstances{}, nil
		}
		names = append(names, name)
	}
	if rows.Err() != nil {
		return ServerInstances{}, nil
	}
	return countServerInstances(names, s.instance), nil
}

// countServerInstances counts the distinct server names among a role's
// client connections, self being this server's.
func countServerInstances(names []string, self string) ServerInstances {
	var out ServerInstances
	seen, sawSelf, unnamed := map[string]bool{}, false, false
	for _, name := range names {
		switch {
		case name == "":
			unnamed = true
		case strings.Contains(strings.ToLower(name), "pgbouncer"):
			out.Pooler = true
		case strings.HasPrefix(name, ServerInstancePrefix) && len(name) >= len(ServerInstancePrefix)+8:
			seen[name] = true
			sawSelf = sawSelf || name == self
		}
	}
	out.Live = len(seen)
	out.Known = sawSelf && !unnamed && !out.Pooler && out.Live > 0
	return out
}
