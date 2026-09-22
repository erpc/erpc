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
}
