package data

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/erpc/erpc/common"
)

// createAWSSession builds an AWS session for the given region using the
// credential source described by auth. Pass auth=nil to use the default
// credential chain (EC2/ECS instance role, env vars, shared credentials file).
// Pass an empty region to let the SDK resolve it from AWS_REGION / IMDS.
//
// The returned session caches credentials internally — for EC2/ECS instance
// roles, IMDS calls are cached for ~6h with proactive refresh.
func createAWSSession(auth *common.AwsAuthConfig, region string) (*session.Session, error) {
	cfg := &aws.Config{}
	if region != "" {
		cfg.Region = aws.String(region)
	}

	if auth == nil {
		return session.NewSession(cfg)
	}

	var creds *credentials.Credentials
	switch auth.Mode {
	case "file":
		creds = credentials.NewSharedCredentials(auth.CredentialsFile, auth.Profile)
	case "env":
		creds = credentials.NewEnvCredentials()
	case "secret":
		creds = credentials.NewStaticCredentials(auth.AccessKeyID, auth.SecretAccessKey, "")
	default:
		return nil, fmt.Errorf("unsupported auth.mode: %q (must be file, env, or secret)", auth.Mode)
	}
	cfg.Credentials = creds
	return session.NewSession(cfg)
}
