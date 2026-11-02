package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

func TestResolveCacheableConsensusPolicies(t *testing.T) {
	assert.Nil(t, ResolveCacheableConsensusPolicies(nil))
	assert.Nil(t, ResolveCacheableConsensusPolicies([]string{}))
	assert.Nil(t, ResolveCacheableConsensusPolicies([]string{""}))
	assert.Equal(t, map[string]struct{}{"default": {}, "strict": {}},
		ResolveCacheableConsensusPolicies([]string{"default", "strict", ""}))
}
