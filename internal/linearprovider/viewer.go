package linearprovider

import "context"

// Viewer is the connected Linear user as get_user reports "me": the name a
// person reads (Name) and their @-handle (DisplayName).
type Viewer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Viewer reads the profile's own Linear user. Only the ID is required;
// either name may be missing.
func (p *Provider) Viewer(ctx context.Context) (Viewer, error) {
	m, e := p.v2Call(ctx, []string{"get_user"}, a("me", "query"))
	if e != nil {
		return Viewer{}, e
	}
	if nested, ok := m["user"].(map[string]any); ok {
		m = nested
	}
	i, e := entityID(m)
	if e != nil {
		return Viewer{}, e
	}
	v := Viewer{ID: i}
	for key, field := range map[string]*string{"name": &v.Name, "displayName": &v.DisplayName} {
		if raw, present := m[key]; present && raw != nil {
			s, ok := raw.(string)
			if !ok || len(s) > 1024 {
				return Viewer{}, fail("invalid_response")
			}
			*field = s
		}
	}
	return v, nil
}
