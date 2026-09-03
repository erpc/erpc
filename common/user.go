package common

// User is the authenticated caller. Beyond identity (Id) it carries the
// per-caller capabilities resolved at authentication time. Capability fields
// are populated ONLY by auth strategies; the trusted-header path
// (NormalizedRequest.SetUserFromTrustedHeader) sets Id alone, so an
// unvalidated header can never grant a capability.
type User struct {
	Id              string
	RateLimitBudget string

	// AllowClientDirectives is the client-directive wildcard pattern granted by
	// the strategy that authenticated this user. Nil means "no strategy-level
	// override" — the project-level pattern applies.
	AllowClientDirectives *string

	// ConsensusPolicies is the set of consensus acceptance grades this caller
	// may be served, granted by the strategy that authenticated them. Nil
	// means "no strategy-level restriction" — any configured grade may be
	// served. A non-nil empty slice permits none.
	ConsensusPolicies *[]string
}

// MayBeServedConsensusPolicy reports whether this caller is allowed to
// receive a consensus result graded under the named acceptance policy.
// A nil user (unauthenticated deployments, internal callers) and a user
// without a strategy-level restriction may be served any grade.
func (u *User) MayBeServedConsensusPolicy(name string) bool {
	if u == nil || u.ConsensusPolicies == nil {
		return true
	}
	for _, allowed := range *u.ConsensusPolicies {
		if allowed == name {
			return true
		}
	}
	return false
}
