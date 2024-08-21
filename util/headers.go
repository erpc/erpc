package util

import (
	"net/http"
	"strings"
)

func ExtractUsefulHeaders(headers http.Header) map[string]interface{} {
	var result = make(map[string]interface{})
	for k, v := range headers {
		kl := strings.ToLower(k)
		if strings.HasPrefix(kl, "x-") ||
			strings.Contains(kl, "trace") ||
			strings.Contains(kl, "debug") ||
			strings.Contains(kl, "correlation") ||
			strings.Contains(kl, "-id") ||
			kl == "content-type" ||
			kl == "content-length" {
			result[kl] = v
		}
	}

	return result
}
