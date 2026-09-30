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

// InternalCaller names a subsystem that originates requests inside erpc, so
// those requests are attributed in metrics and usage exports instead of
// reporting user "n/a" and agent "unknown". Values are shared and read-only.
type InternalCaller struct {
	user *User
}

func newInternalCaller(name string) InternalCaller {
	return InternalCaller{user: &User{Id: name}}
}

var (
	// InternalCallerStatePoller: head, finality, syncing and block-availability polls.
	InternalCallerStatePoller = newInternalCaller("erpc-state-poller")
	// InternalCallerIntegrity: integrity header fetches and state probes.
	InternalCallerIntegrity = newInternalCaller("erpc-integrity")
	// InternalCallerDetection: chain id and config detection at upstream bootstrap.
	InternalCallerDetection = newInternalCaller("erpc-detection")
)
