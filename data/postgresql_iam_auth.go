package data

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/rds/rdsutils"
	"github.com/erpc/erpc/common"
	"github.com/jackc/pgx/v4"
)

// newRDSBeforeConnect returns a pgxpool BeforeConnect hook that injects a fresh
// RDS IAM auth token as the connection password on every new pool connection.
//
// The IAM token is valid for 15 minutes, but only matters at connection
// establishment time — existing connections are not re-authenticated. pgxpool's
// MaxConnLifetime (currently 5h in postgresql.go) naturally rotates connections,
// and each new connection calls this hook to mint a fresh token.
func newRDSBeforeConnect(
	sess *session.Session,
	cfg *common.PostgreSQLIAMAuthConfig,
) func(ctx context.Context, cc *pgx.ConnConfig) error {
	return func(_ context.Context, cc *pgx.ConnConfig) error {
		token, err := rdsutils.BuildAuthToken(
			cfg.Endpoint, // host:port
			aws.StringValue(sess.Config.Region),
			cfg.DBUser,
			sess.Config.Credentials,
		)
		if err != nil {
			return fmt.Errorf("rds iam: build auth token: %w", err)
		}
		cc.Password = token
		cc.User = cfg.DBUser
		return nil
	}
}
