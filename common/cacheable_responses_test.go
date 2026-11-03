package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func init() {
	util.ConfigureTestLogger()
}

func TestAllowsCacheWrite(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		strategy CachingStrategy
		mark     *bool
		want     bool
	}{
		{"defaultAllow_nil", CachingStrategyDefaultAllow, nil, true},
		{"defaultAllow_true", CachingStrategyDefaultAllow, &yes, true},
		{"defaultAllow_false", CachingStrategyDefaultAllow, &no, false},
		{"emptyStrategy_nil_treatedAsDefaultAllow", "", nil, true},
		{"emptyStrategy_false", "", &no, false},
		{"defaultIgnore_nil", CachingStrategyDefaultIgnore, nil, false},
		{"defaultIgnore_true", CachingStrategyDefaultIgnore, &yes, true},
		{"defaultIgnore_false", CachingStrategyDefaultIgnore, &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AllowsCacheWrite(tc.strategy, tc.mark))
		})
	}
}

func TestCacheConfig_SetDefaults_CachingStrategy(t *testing.T) {
	c := &CacheConfig{}
	require.NoError(t, c.SetDefaults())
	assert.Equal(t, CachingStrategyDefaultAllow, c.CachingStrategy)

	c2 := &CacheConfig{CachingStrategy: CachingStrategyDefaultIgnore}
	require.NoError(t, c2.SetDefaults())
	assert.Equal(t, CachingStrategyDefaultIgnore, c2.CachingStrategy)
}

func TestCacheConfig_Validate_CachingStrategy(t *testing.T) {
	require.NoError(t, (&CacheConfig{CachingStrategy: CachingStrategyDefaultAllow}).Validate())
	require.NoError(t, (&CacheConfig{CachingStrategy: CachingStrategyDefaultIgnore}).Validate())
	require.NoError(t, (&CacheConfig{}).Validate(), "empty pre-SetDefaults is ok")
	err := (&CacheConfig{CachingStrategy: "nope"}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cachingStrategy")
}

func TestConsensusPolicy_CacheableResponses_OmitDefaults(t *testing.T) {
	const src = `
maxParticipants: 3
agreementThreshold: 2
policies:
  fallback:
    extends: default
    requiredParticipants: []
  explicitTrue:
    maxParticipants: 3
    agreementThreshold: 2
    cacheableResponses: true
  stock:
    maxParticipants: 3
    agreementThreshold: 2
`
	var c ConsensusPolicyConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &c))
	require.NoError(t, c.SetDefaults())

	require.NotNil(t, c.CacheableResponses)
	assert.True(t, *c.CacheableResponses, "inline omit → true")

	fb := c.Policies["fallback"]
	require.NotNil(t, fb.CacheableResponses)
	assert.False(t, *fb.CacheableResponses, "named omit (extends default) → false, not inherited inline true")

	et := c.Policies["explicitTrue"]
	require.NotNil(t, et.CacheableResponses)
	assert.True(t, *et.CacheableResponses, "explicit true kept")

	stock := c.Policies["stock"]
	require.NotNil(t, stock.CacheableResponses)
	assert.False(t, *stock.CacheableResponses, "named omit without extends → false")
}

func TestNormalizedResponse_CacheableMark(t *testing.T) {
	r := NewNormalizedResponse()
	assert.Nil(t, r.Cacheable())

	yes := true
	r.SetCacheable(&yes)
	require.NotNil(t, r.Cacheable())
	assert.True(t, *r.Cacheable())

	// Stored copy must not alias the caller's variable.
	yes = false
	assert.True(t, *r.Cacheable())

	no := false
	r.SetCacheable(&no)
	assert.False(t, *r.Cacheable())

	r.SetCacheable(nil)
	assert.Nil(t, r.Cacheable())

	assert.Nil(t, (*NormalizedResponse)(nil).Cacheable())
	assert.Nil(t, (*NormalizedResponse)(nil).SetCacheable(&yes))
}
