package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBlockParameter(t *testing.T) {
	tests := []struct {
		name          string
		input         interface{}
		expectedNum   string
		expectedHash  []byte
		expectedError bool
	}{
		{
			name:          "string block number hex",
			input:         "0x123",
			expectedNum:   "0x123",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name:          "string block tag latest",
			input:         "latest",
			expectedNum:   "latest",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name:          "string block hash",
			input:         "0xde8d803a10bfc89a90b3c91753d271cb5aae5231267072205d35d24409d7528f",
			expectedNum:   "",
			expectedHash:  []byte{0xde, 0x8d, 0x80, 0x3a, 0x10, 0xbf, 0xc8, 0x9a, 0x90, 0xb3, 0xc9, 0x17, 0x53, 0xd2, 0x71, 0xcb, 0x5a, 0xae, 0x52, 0x31, 0x26, 0x70, 0x72, 0x20, 0x5d, 0x35, 0xd2, 0x44, 0x09, 0xd7, 0x52, 0x8f},
			expectedError: false,
		},
		{
			name:          "float64 block number",
			input:         float64(123),
			expectedNum:   "0x7b",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name:          "int64 block number",
			input:         int64(123),
			expectedNum:   "0x7b",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name:          "uint64 block number",
			input:         uint64(123),
			expectedNum:   "0x7b",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name: "object with blockHash",
			input: map[string]interface{}{
				"blockHash": "0xde8d803a10bfc89a90b3c91753d271cb5aae5231267072205d35d24409d7528f",
			},
			expectedNum:   "",
			expectedHash:  []byte{0xde, 0x8d, 0x80, 0x3a, 0x10, 0xbf, 0xc8, 0x9a, 0x90, 0xb3, 0xc9, 0x17, 0x53, 0xd2, 0x71, 0xcb, 0x5a, 0xae, 0x52, 0x31, 0x26, 0x70, 0x72, 0x20, 0x5d, 0x35, 0xd2, 0x44, 0x09, 0xd7, 0x52, 0x8f},
			expectedError: false,
		},
		{
			name: "object with blockNumber",
			input: map[string]interface{}{
				"blockNumber": "0x123",
			},
			expectedNum:   "0x123",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name: "object with blockTag",
			input: map[string]interface{}{
				"blockTag": "latest",
			},
			expectedNum:   "latest",
			expectedHash:  nil,
			expectedError: false,
		},
		{
			name:          "empty object",
			input:         map[string]interface{}{},
			expectedNum:   "",
			expectedHash:  nil,
			expectedError: true,
		},
		{
			name:          "invalid type",
			input:         []string{"invalid"},
			expectedNum:   "",
			expectedHash:  nil,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blockNumber, blockHash, err := ParseBlockParameter(tt.input)

			if tt.expectedError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedNum, blockNumber)
			assert.Equal(t, tt.expectedHash, blockHash)
		})
	}
}
