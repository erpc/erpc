package util

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

func RedactEndpoint(endpoint string) string {
	// Calculate hash of the entire original endpoint
	hasher := sha256.New()
	hasher.Write(Str2Mem(endpoint))
	hash := hex.EncodeToString(hasher.Sum(nil))

	// Parse the endpoint URL
	parsedURL, err := url.Parse(endpoint)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		// If parsing fails or the URL is incomplete, return just the hash
		return "redacted=" + hash[:5]
	}

	// Construct the redacted endpoint
	var redactedEndpoint string
	if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" || parsedURL.Scheme == "ws" || parsedURL.Scheme == "wss" {
		redactedEndpoint = parsedURL.Scheme + "://" + parsedURL.Host + "#redacted=" + hash[:5]
	} else if strings.HasSuffix(parsedURL.Scheme, "envio") {
		redactedEndpoint = parsedURL.Scheme + "://" + parsedURL.Host
	} else {
		redactedEndpoint = parsedURL.Scheme + "#redacted=" + hash[:5]
	}

	return redactedEndpoint
}
