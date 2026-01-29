package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestQuicknodeFilterParams(t *testing.T) {
	t.Parallel()
	vendor := CreateQuicknodeVendor().(*QuicknodeVendor)

	t.Run("extracts both tag IDs and labels from interface arrays", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey":    "test-key",
			"tagIds":    []interface{}{123, 456},
			"tagLabels": []interface{}{"production", "staging"},
		}
		result := vendor.extractFilterParams(settings)
		assert.Equal(t, []int{123, 456}, result.TagIDs)
		assert.Equal(t, []string{"production", "staging"}, result.TagLabels)
	})

	t.Run("extracts both tag IDs and labels from concrete types", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey":    "test-key",
			"tagIds":    []int{123, 456},
			"tagLabels": []string{"production", "staging"},
		}
		result := vendor.extractFilterParams(settings)
		assert.Equal(t, []int{123, 456}, result.TagIDs)
		assert.Equal(t, []string{"production", "staging"}, result.TagLabels)
	})

	t.Run("handles empty settings", func(t *testing.T) {
		settings := common.VendorSettings{"apiKey": "test-key"}
		result := vendor.extractFilterParams(settings)
		assert.Empty(t, result.TagIDs)
		assert.Empty(t, result.TagLabels)
	})
}
