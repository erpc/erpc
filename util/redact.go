package util

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
)

func RedactEndpoint(endpoint string) string {
	// Calculate hash of the entire original endpoint
	hasher := sha256.New()
	hasher.Write([]byte(endpoint))
	hash := hex.EncodeToString(hasher.Sum(nil))

	// Parse the endpoint URL
	parsedURL, err := url.Parse(endpoint)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		// If parsing fails or the URL is incomplete, return just the hash
		return hash
	}

	// Construct the redacted endpoint
	redactedEndpoint := parsedURL.Scheme + "://" + parsedURL.Host + "#hash=" + hash

	return redactedEndpoint
}
