package common

import (
	"fmt"
	"strings"
)

// CommitmentLevel is the caller's explicitly requested Solana commitment, as
// used by failsafe matchCommitment. It is NOT the network-injected default and
// NOT Solana's effective/server-side default — those live in architecture/svm
// (resolveCommitment / effectiveCommitment). Empty matchCommitment is a
// wildcard; matching is exact (confirmed does not imply processed).
type CommitmentLevel string

const (
	CommitmentNone      CommitmentLevel = "none"
	CommitmentProcessed CommitmentLevel = "processed"
	CommitmentConfirmed CommitmentLevel = "confirmed"
	CommitmentFinalized CommitmentLevel = "finalized"
	// CommitmentUnknown is request-side only: malformed or unsupported values
	// extracted from params. It is never a valid matchCommitment config token.
	CommitmentUnknown CommitmentLevel = "unknown"
)

func (c CommitmentLevel) String() string {
	return string(c)
}

func (c CommitmentLevel) MarshalYAML() (interface{}, error) {
	return string(c), nil
}

func (c CommitmentLevel) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(string(c))
}

func (c *CommitmentLevel) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := ParseCommitmentLevel(s)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

func (c *CommitmentLevel) UnmarshalJSON(data []byte) error {
	var s string
	if err := SonicCfg.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := ParseCommitmentLevel(s)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// ParseCommitmentLevel maps a config or wire string to a CommitmentLevel.
// Config validation only allows none|processed|confirmed|finalized; unknown
// is produced by the request extractor for bad values, not by this parser
// when used for config (it rejects unknown).
func ParseCommitmentLevel(s string) (CommitmentLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return CommitmentNone, nil
	case "processed":
		return CommitmentProcessed, nil
	case "confirmed":
		return CommitmentConfirmed, nil
	case "finalized":
		return CommitmentFinalized, nil
	default:
		return "", fmt.Errorf("invalid commitment level: %s", s)
	}
}

// ParseRequestedCommitment maps an extracted wire value to a CommitmentLevel.
// Empty → none; known tokens → that level; anything else → unknown.
func ParseRequestedCommitment(s string) CommitmentLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return CommitmentNone
	case "processed":
		return CommitmentProcessed
	case "confirmed":
		return CommitmentConfirmed
	case "finalized":
		return CommitmentFinalized
	default:
		return CommitmentUnknown
	}
}

// MatchCommitments reports whether two matchCommitment lists overlap for
// defaults merging. Empty (nil or len=0) matches any commitment list.
func MatchCommitments(a, b []CommitmentLevel) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
