package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewProvidersRegistry(t *testing.T) {
	logger := zerolog.Nop()
	vr := NewVendorsRegistry()

	t.Run("builds one provider per config", func(t *testing.T) {
		cfgs := []*common.ProviderConfig{
			{Id: "gs", Vendor: "goldsky"},
			{Id: "rm", Vendor: "routemesh"},
		}
		pr, err := NewProvidersRegistry(&logger, vr, cfgs, nil)
		require.NoError(t, err)
		all := pr.GetAllProviders()
		require.Len(t, all, 2)
		assert.Equal(t, "gs", all[0].Id())
		assert.Equal(t, "rm", all[1].Id())
		assert.Equal(t, "goldsky", all[0].vendor.Name())
		assert.Equal(t, "routemesh", all[1].vendor.Name())
	})

	t.Run("no configs yields empty registry", func(t *testing.T) {
		pr, err := NewProvidersRegistry(&logger, vr, nil, nil)
		require.NoError(t, err)
		assert.Empty(t, pr.GetAllProviders())
	})

	t.Run("unknown vendor lists supported vendors", func(t *testing.T) {
		cfgs := []*common.ProviderConfig{{Id: "x", Vendor: "nope"}}
		pr, err := NewProvidersRegistry(&logger, vr, cfgs, nil)
		assert.Nil(t, pr)
		assert.ErrorContains(t, err, "vendor 'nope' not found for provider 'x'")
		assert.ErrorContains(t, err, "goldsky")
	})
}
