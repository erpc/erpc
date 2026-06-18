package evm

import (
	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
)

// integrityCheckSet maps request directives to the set of integrity checks to
// run, bridging the existing directive flags to the unified check ids. This is
// the single place that assembles a CheckSet for the engine; as more checks are
// migrated into the integrity package — and as the level/header config model
// lands — this is where their enablement is wired.
func integrityCheckSet(dirs *common.RequestDirectives) integrity.CheckSet {
	cs := integrity.CheckSet{}

	// Always-on sanity check: provably-impossible logIndex/transactionIndex.
	cs.Enable("indexMagnitude", nil)

	return cs
}
