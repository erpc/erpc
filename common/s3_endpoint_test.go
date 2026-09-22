package common

import "testing"

func TestS3FlushConfigEndpointValidation(t *testing.T) {
	cases := []struct {
		endpoint string
		wantErr  bool
	}{
		{"", false},
		{"https://t3.storage.dev", false},
		{"http://minio.local:9000", false},
		{"t3.storage.dev", true},   // missing scheme
		{"ftp://weird.host", true}, // non-http scheme
		{"https://", true},         // no host
	}
	for _, tc := range cases {
		cfg := &S3FlushConfig{Endpoint: tc.endpoint}
		err := cfg.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("endpoint %q: got err=%v, wantErr=%v", tc.endpoint, err, tc.wantErr)
		}
	}
}

func TestMisbehaviorsDestinationCopyKeepsEndpoint(t *testing.T) {
	orig := &MisbehaviorsDestinationConfig{
		Type: MisbehaviorsDestinationTypeS3,
		Path: "s3://bucket/prefix/",
		S3:   &S3FlushConfig{Endpoint: "https://t3.storage.dev", Region: "auto"},
	}
	copied := orig.Copy()
	if copied.S3 == nil || copied.S3.Endpoint != orig.S3.Endpoint {
		t.Fatalf("Copy dropped S3.Endpoint: %+v", copied.S3)
	}
}
