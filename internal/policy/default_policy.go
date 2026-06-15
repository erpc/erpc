package policy

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

//go:embed default_policy.js
var defaultPolicyJS string

// DefaultPolicySource is the JS source for the policy applied when a user
// has not supplied `selectionPolicy.eval`. It is the canonical reference
// the admin endpoint `GET /admin/selection/default-policy` returns.
func DefaultPolicySource() string {
	return strings.TrimSpace(defaultPolicyJS)
}

// upgradeDefaultPolicy swaps the trivial identity placeholder
// (`common.DefaultSelectionPolicySource`) for the rich default-policy.js
// at engine-register time. common/ can't reach this package, so the
// upgrade must happen here.
func upgradeDefaultPolicy(cfg *common.SelectionPolicyConfig) error {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(cfg.EvalFunc) != strings.TrimSpace(common.DefaultSelectionPolicySource) {
		return nil
	}
	cfg.EvalFunc = DefaultPolicySource()
	cfg.EvalFuncOriginal = cfg.EvalFunc
	program, err := common.CompileProgram(cfg.EvalFunc)
	if err != nil {
		return fmt.Errorf("compile default selection policy: %w", err)
	}
	cfg.CompiledProgram = program
	return nil
}
