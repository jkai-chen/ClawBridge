package linearprovider

import "context"

// SetProjectStatus moves a project to a project status by its ID, through
// save_project {id, state} (A4, STATUS-FLOW §11.3). It sends an ID only,
// never a name or a type, so Linear resolves nothing; the caller reads the
// project back to learn whether it moved.
func (p *Provider) SetProjectStatus(ctx context.Context, project, status string) error {
	if !ValidID(project) || !ValidID(status) {
		return fail("invalid")
	}
	args := []arg{a(project, "id"), a(status, "state")}
	if _, _, e := p.v2Negotiation([]string{"save_project"}, args); e != nil {
		return fail("capability_unavailable")
	}
	_, e := p.v2Call(ctx, []string{"save_project"}, args...)
	return e
}
