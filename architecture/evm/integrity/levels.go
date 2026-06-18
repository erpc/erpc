package integrity

// Level is the single front-door preset over the check catalog (spec §4/§9):
// the one knob most operators set. Higher levels are supersets of lower ones.
type Level string

const (
	LevelOff           Level = "off"
	LevelIntrinsic     Level = "intrinsic"
	LevelCorroborated  Level = "corroborated"
	LevelAuthoritative Level = "authoritative"
)

// rank orders the levels so a preset can be expressed as "this row and every
// lower row". off/unknown is 0 (enables nothing).
func (l Level) rank() int {
	switch l {
	case LevelIntrinsic:
		return 1
	case LevelCorroborated:
		return 2
	case LevelAuthoritative:
		return 3
	default:
		return 0
	}
}

// levelMembership is the single auditable source of truth for which checks each
// level *introduces*. A level enables the union of its own row and all lower
// rows (intrinsic ⊂ corroborated ⊂ authoritative):
//
//   - intrinsic     — pure self-consistency; no upstream cost, always safe.
//   - corroborated  — compare against ground truth the caller/feeder already
//     supplied; no force-fetch.
//   - authoritative — force-fetch the canonical block to corroborate against.
//
// Every registered check id must appear in exactly one row, and no row may name
// an unknown id — both enforced by TestLevelMembershipCoversAllChecks.
var levelMembership = map[Level][]string{
	LevelIntrinsic: {
		"indexMagnitude", "schemaConformance", "sameBlockHash", "txHashUniqueness",
		"transactionIndexConsistency", "logFieldShapes", "logMetadata",
		"bloomEmptiness", "bloomMatch", "logIndexContiguity",
		"transactionsRootConsistency", "headerFieldShapes", "txFieldUniqueness", "txBlockInfo",
		"blockHashRecompute", "senderRecovery", "transactionsRootRecompute",
	},
	LevelCorroborated: {
		"expectedBlock", "receiptsCount", "receiptTransactionMatch",
	},
	LevelAuthoritative: {
		"receiptVsBlock",
	},
}

// CheckSetForLevel returns the checks a level enables: the union of its row and
// all lower rows. This is the single mapping from the front-door knob to the
// check vocabulary; everything else (directives, headers, profiles) composes
// over the resulting set.
func CheckSetForLevel(level Level) CheckSet {
	cs := CheckSet{}
	r := level.rank()
	if r == 0 {
		return cs
	}
	for lvl, ids := range levelMembership {
		if lvl.rank() > r {
			continue
		}
		for _, id := range ids {
			cs.Enable(id, nil)
		}
	}
	return cs
}
