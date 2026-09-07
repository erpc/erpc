package common

// CordonEntry is one owner's cordon on an (upstream, method) cell. Shared by
// the in-process health tracker and the shared-state store that persists
// operator cordons across replicas.
type CordonEntry struct {
	Reason       string `json:"reason"`
	CordonedAtMs int64  `json:"cordonedAtMs"`
}
