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
			strings.Contains(kl, "request-id") ||
			strings.Contains(kl, "span") ||
			strings.Contains(kl, "parent") ||
			strings.Contains(kl, "error") ||
			strings.Contains(kl, "rate-limit") ||
			kl == "content-type" ||
			kl == "content-length" ||
			kl == "server" ||
			kl == "date" ||
			kl == "etag" ||
			kl == "status" ||
			kl == "retry-after" ||
			kl == "cache-control" {
			result[kl] = r.Header.Get(k)
		}
	}

	return result
}
