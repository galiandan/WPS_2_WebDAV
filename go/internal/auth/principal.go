package auth

import "context"

const InstallationID = "installation"

type Permissions struct {
	Read   bool `json:"read"`
	Upload bool `json:"upload"`
	Delete bool `json:"delete"`
}

type Principal struct {
	ID            string      `json:"id,omitempty"`
	Username      string      `json:"username"`
	Role          string      `json:"role,omitempty"`
	PolicyVersion uint64      `json:"policy_version,omitempty"`
	RootPath      string      `json:"root_path,omitempty"`
	RootID        string      `json:"root_id,omitempty"`
	RootBinding   string      `json:"root_binding,omitempty"`
	Permissions   Permissions `json:"permissions"`
}

func (p Principal) IsAdmin() bool { return p.ID == InstallationID && p.Role == "admin" }

type Provider interface {
	Authenticate(username, password string) (Principal, error)
	LookupUsername(string) (Principal, bool)
	LookupID(string) (Principal, bool)
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}
func PrincipalFromContext(ctx context.Context) (Principal, bool) { return PrincipalFrom(ctx) }
