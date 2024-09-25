package util

import (
	"strings"

	"net/http"
)

func ExtractUsefulHeaders(r *http.Response) map[string]interface{} {
	var result = make(map[string]interface{})
	for k := range r.Header {
		kl := strings.ToLower(k)
		if strings.HasPrefix(kl, "x-") ||
			strings.Contains(kl, "trace") ||
			strings.Contains(kl, "debug") ||
			strings.Contains(kl, "correlation") ||
			strings.Contains(kl, "-id") ||
			kl == "content-type" ||
			kl == "content-length" {
			result[kl] = r.Header.Get(k)
		}
	}

	return result
}
