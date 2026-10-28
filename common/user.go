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

	// Auth is the name of the auth strategy that authenticated this caller
	// (feature §4.3). Stamped ONLY by the auth registry from the strategy
	// config — never from a client-controlled header. Empty when the strategy
	// is unnamed. The trusted-header path never sets it. The consensus-policy
	// selector compares this string in operator evals (ctx.user.auth).
	Auth string
}
