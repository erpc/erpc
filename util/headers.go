package util

import (
	"net/http"
	"strings"
)

func ExtractUsefulHeaders(headers http.Header) map[string]interface{} {
	var result = make(map[string]interface{})
	for k, v := range headers {
		if strings.HasPrefix(k, "x-") ||
			strings.Contains(k, "trace") ||
			strings.Contains(k, "debug") ||
			strings.Contains(k, "correlation") ||
			strings.Contains(k, "-id") || 
			k == "content-type" ||
			k == "content-length" {
			result[k] = v
		}
	}

	return result
}