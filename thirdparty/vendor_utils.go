package thirdparty

import (
	"fmt"
	"net/url"
)

// validateChainsURL returns a non-nil error if rawURL is structurally invalid
// (bad scheme, empty host). A reachable-but-failing URL is a network error,
// not a config error, and is handled separately by the cold-start fallback.
func validateChainsURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid chainsUrl %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid chainsUrl %q: scheme must be http or https", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid chainsUrl %q: host is empty", rawURL)
	}
	return nil
}
