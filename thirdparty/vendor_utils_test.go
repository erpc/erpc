package thirdparty

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateChainsURL(t *testing.T) {
	cases := []struct {
		url      string
		contains string
	}{
		{"https://chains.example.com/list", ""},
		{"http://127.0.0.1:8080/chains", ""},
		{"ftp://chains.example.com", "scheme must be http or https"},
		{"chains.example.com/list", "scheme must be http or https"},
		{"https:///no-host", "host is empty"},
		{"https://[::1", "invalid chainsUrl"},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			err := validateChainsURL(c.url)
			if c.contains == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, c.contains)
			}
		})
	}
}
