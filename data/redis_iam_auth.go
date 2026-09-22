package data

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/erpc/erpc/common"
)

const (
	elastiCacheService = "elasticache"
	iamTokenValidity   = 15 * time.Minute

	// ElastiCache forcibly disconnects IAM-authenticated connections after 12h.
	// Rotate proactively at 11h so every new connection gets a fresh token
	// well before the server-side cliff.
	iamRedisConnMaxLifetime       = 11 * time.Hour
	iamRedisConnMaxLifetimeJitter = 30 * time.Minute
)

// generateElastiCacheIAMToken produces a SigV4-presigned auth token for
// ElastiCache IAM authentication. The output is the presigned URL with the URL
// scheme stripped, which is used as the Redis AUTH password.
//
// Ref: https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/auth-iam.html
//
// Note on URL scheme: SigV4 does NOT include the URL scheme in the canonical
// request, so http:// and https:// produce identical signatures and tokens.
// We use https:// to match the AWS Go SDK convention in rdsutils.BuildAuthToken
// (service/rds/rdsutils/connect.go), which documents "the scheme is arbitrary
// and is only needed because validation of the URL requires one." We strip both
// prefixes defensively in case the SDK normalizes the URL differently.
func generateElastiCacheIAMToken(sess *session.Session, cfg *common.RedisIAMAuthConfig) (string, error) {
	// AWS lowercases cache names at creation time; SetDefaults normalizes this
	// already, but be defensive in case the struct is mutated after config load.
	cacheName := strings.ToLower(cfg.CacheName)

	req, err := http.NewRequest(http.MethodGet, "https://"+cacheName+"/", nil)
	if err != nil {
		return "", fmt.Errorf("elasticache iam: build request: %w", err)
	}

	q := req.URL.Query()
	q.Set("Action", "connect")
	q.Set("User", cfg.UserID)
	req.URL.RawQuery = q.Encode()

	signer := v4.NewSigner(sess.Config.Credentials)
	if _, err = signer.Presign(req, nil, elastiCacheService, aws.StringValue(sess.Config.Region), iamTokenValidity, time.Now()); err != nil {
		return "", fmt.Errorf("elasticache iam: presign: %w", err)
	}

	// Strip whichever scheme the SDK produced — same defensive pattern as
	// rdsutils.BuildAuthToken.
	u := req.URL.String()
	if strings.HasPrefix(u, "https://") {
		return u[len("https://"):], nil
	}
	if strings.HasPrefix(u, "http://") {
		return u[len("http://"):], nil
	}
	return u, nil
}

// newElastiCacheCredentialsProvider returns a go-redis CredentialsProviderContext
// callback. go-redis calls this function when establishing each new physical
// connection, so combined with ConnMaxLifetime ≈ 11h, every new connection
// receives a fresh token that is well within the 15-minute validity window.
func newElastiCacheCredentialsProvider(
	sess *session.Session,
	cfg *common.RedisIAMAuthConfig,
) func(ctx context.Context) (string, string, error) {
	return func(_ context.Context) (username, password string, err error) {
		token, err := generateElastiCacheIAMToken(sess, cfg)
		if err != nil {
			return "", "", err
		}
		return cfg.UserID, token, nil
	}
}
