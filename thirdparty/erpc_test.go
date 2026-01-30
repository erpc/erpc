package thirdparty

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErpcVendor_parseEndpointURL(t *testing.T) {
	t.Parallel()
	v := &ErpcVendor{}
	chainId := int64(1)

	testCases := []struct {
		name           string
		endpoint       string
		secret         string
		expectedScheme string
		expectedHost   string
		expectedPath   string
		expectedQuery  string
	}{
		{
			name:           "erpc:// without port defaults to https",
			endpoint:       "erpc://domain.com",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with port 443 uses https",
			endpoint:       "erpc://domain.com:443",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com:443",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with port 80 uses http",
			endpoint:       "erpc://domain.com:80",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:80",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with custom port uses http",
			endpoint:       "erpc://domain.com:8545",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:8545",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with query params preserves them",
			endpoint:       "erpc://domain.com?param1=value1&param2=value2",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "param1=value1&param2=value2",
		},
		{
			name:           "erpc:// with query params and secret adds secret",
			endpoint:       "erpc://domain.com?param1=value1",
			secret:         "mysecret",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "param1=value1&secret=mysecret",
		},
		{
			name:           "erpc:// with port 443 and query params",
			endpoint:       "erpc://domain.com:443?param1=value1&param2=value2",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com:443",
			expectedPath:   "/1",
			expectedQuery:  "param1=value1&param2=value2",
		},
		{
			name:           "plain domain without port defaults to https",
			endpoint:       "domain.com",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "plain domain with port 80 uses http",
			endpoint:       "domain.com:80",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:80",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "http:// URL preserves scheme",
			endpoint:       "http://domain.com:8080",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:8080",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "https:// URL preserves scheme",
			endpoint:       "https://domain.com",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "https:// URL with existing path appends chainId",
			endpoint:       "https://domain.com/api/v1",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/api/v1/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with path and query params",
			endpoint:       "erpc://domain.com:8545/rpc?apikey=xyz&timeout=30",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:8545",
			expectedPath:   "/rpc/1",
			expectedQuery:  "apikey=xyz&timeout=30",
		},
		{
			name:           "erpc:// with secret in URL and separate secret param",
			endpoint:       "erpc://domain.com?secret=url_secret",
			secret:         "param_secret",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "secret=param_secret",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			parsedURL, err := v.parseEndpointURL(tc.endpoint, tc.secret, chainId)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedScheme, parsedURL.Scheme)
			assert.Equal(t, tc.expectedHost, parsedURL.Host)
			assert.Equal(t, tc.expectedPath, parsedURL.Path)
			assert.Equal(t, tc.expectedQuery, parsedURL.RawQuery)
		})
	}
}
