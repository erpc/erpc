package common

import "encoding/hex"

// ConsensusPolicyObjectKeyComponent is the injective encoding substituted for
// the `{policy}` placeholder in misbehavior export file/object keys.
//
// It MUST stay injective over the raw policy name: sanitizeForFilename is
// non-injective (e.g. "a/b" and "a_b" both become "a_b"), so using it here
// would let two named policies that share an S3/file destination overwrite
// each other on same-millisecond flushes.
func ConsensusPolicyObjectKeyComponent(policyName string) string {
	return hex.EncodeToString([]byte(policyName))
}
