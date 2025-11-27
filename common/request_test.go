package common

import (
	"net/http"
	"net/url"
	"testing"
)

func TestEnrichFromHttpHandlesBloomValidationHeaders(t *testing.T) {
	req := NewNormalizedRequest(nil)
	headers := http.Header{}
	headers.Set(headerDirectiveValidateLogsBloomEmpty, "true")

	req.EnrichFromHttp(headers, nil, UserAgentTrackingModeSimplified)

	dir := req.Directives()
	if dir == nil {
		t.Fatalf("expected directives to be initialized when headers are provided")
	}
	if !dir.ValidateLogsBloomEmptiness {
		t.Fatalf("expected ValidateLogsBloomEmptiness to be true")
	}
}

func TestEnrichFromHttpHandlesBloomValidationQueryParams(t *testing.T) {
	req := NewNormalizedRequest(nil)
	query := url.Values{}
	query.Set(queryDirectiveValidateLogsBloomMatch, "true")

	req.EnrichFromHttp(nil, query, UserAgentTrackingModeSimplified)

	dir := req.Directives()
	if dir == nil {
		t.Fatalf("expected directives to be initialized when query params are provided")
	}
	if !dir.ValidateLogsBloomMatch {
		t.Fatalf("expected ValidateLogsBloomMatch to be true")
	}
}
