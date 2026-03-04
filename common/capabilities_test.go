package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeCapabilityTags(t *testing.T) {
	assert.Equal(t, []string{"trace", "archive"}, NormalizeCapabilityTags([]string{" Trace ", "archive", "TRACE", ""}))
	assert.Nil(t, NormalizeCapabilityTags(nil))
	assert.Nil(t, NormalizeCapabilityTags([]string{"", "  "}))
}

func TestHasAllCapabilities(t *testing.T) {
	assert.True(t, HasAllCapabilities([]string{"trace", "archive"}, []string{"trace"}))
	assert.True(t, HasAllCapabilities([]string{"TRACE", "archive"}, []string{"trace"}))
	assert.False(t, HasAllCapabilities([]string{"archive"}, []string{"trace"}))
	assert.True(t, HasAllCapabilities([]string{"archive"}, nil))
}

func TestIsValidCapabilityTag(t *testing.T) {
	assert.True(t, IsValidCapabilityTag("trace"))
	assert.True(t, IsValidCapabilityTag("evm.trace-v2"))
	assert.True(t, IsValidCapabilityTag(" trace "))
	assert.False(t, IsValidCapabilityTag(""))
	assert.False(t, IsValidCapabilityTag("trace!"))
}
