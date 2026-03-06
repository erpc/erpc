package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/net/http2"
)

const (
	S3DriverName = "s3"

	// S3 metadata key for TTL tracking (AWS SDK lowercases metadata keys)
	s3MetaExpiresAt = "expires-at"
)

var s3SharedReadClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        2048,
		MaxIdleConnsPerHost: 2048,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     120 * time.Second,
	},
}
var s3SharedWriteClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     120 * time.Second,
	},
}

func init() {
	_ = http2.ConfigureTransport(s3SharedReadClient.Transport.(*http.Transport))
	_ = http2.ConfigureTransport(s3SharedWriteClient.Transport.(*http.Transport))
}

var _ Connector = (*S3Connector)(nil)

type S3Connector struct {
	id                  string
	logger              *zerolog.Logger
	initializer         *util.Initializer
	readClient          *s3.Client
	writeClient         *s3.Client
	bucket              string
	keyPrefix           string
	initTimeout         time.Duration
	getTimeout          time.Duration
	setTimeout          time.Duration
	evmFinalityMetadata *common.EvmFinalityS3Metadata
}

func NewS3Connector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.S3ConnectorConfig,
) (*S3Connector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating s3 connector")

	connector := &S3Connector{
		id:                  id,
		logger:              &lg,
		bucket:              cfg.Bucket,
		keyPrefix:           cfg.KeyPrefix,
		initTimeout:         cfg.InitTimeout.Duration(),
		getTimeout:          cfg.GetTimeout.Duration(),
		setTimeout:          cfg.SetTimeout.Duration(),
		evmFinalityMetadata: cfg.EvmFinalityS3Metadata,
	}

	connector.initializer = util.NewInitializer(ctx, &lg, nil)

	connectTask := util.NewBootstrapTask(
		fmt.Sprintf("s3-connect/%s", id),
		func(ctx context.Context) error {
			return connector.connectTask(ctx, cfg)
		},
	)

	if err := connector.initializer.ExecuteTasks(ctx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize S3 on first attempt (will retry in background)")
		return connector, nil
	}

	return connector, nil
}

func (c *S3Connector) connectTask(ctx context.Context, cfg *common.S3ConnectorConfig) error {
	awsCfg, err := createS3Config(ctx, cfg)
	if err != nil {
		return common.NewTaskFatal(err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		})
	}
	s3Opts = append(s3Opts, func(o *s3.Options) {
		o.RetryMaxAttempts = cfg.MaxRetries
	})

	writeOpts := make([]func(*s3.Options), len(s3Opts)+1)
	copy(writeOpts, s3Opts)
	writeOpts[len(s3Opts)] = func(o *s3.Options) { o.HTTPClient = s3SharedWriteClient }
	c.writeClient = s3.NewFromConfig(awsCfg, writeOpts...)

	readOpts := make([]func(*s3.Options), len(s3Opts)+1)
	copy(readOpts, s3Opts)
	readOpts[len(s3Opts)] = func(o *s3.Options) { o.HTTPClient = s3SharedReadClient }
	c.readClient = s3.NewFromConfig(awsCfg, readOpts...)

	_, err = c.readClient.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	if err != nil {
		return fmt.Errorf("unable to access S3 bucket %s: %w", c.bucket, err)
	}

	c.logger.Debug().Str("bucket", c.bucket).Msg("s3 bucket is accessible")
	return nil
}

func createS3Config(ctx context.Context, cfg *common.S3ConnectorConfig) (aws.Config, error) {
	if cfg == nil || cfg.Region == "" {
		return aws.Config{}, fmt.Errorf("missing region for store.s3")
	}

	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts, awsconfig.WithRegion(cfg.Region))

	if cfg.Auth == nil {
		return awsconfig.LoadDefaultConfig(ctx, opts...)
	}

	switch cfg.Auth.Mode {
	case "file":
		opts = append(opts,
			awsconfig.WithSharedConfigFiles([]string{cfg.Auth.CredentialsFile}),
			awsconfig.WithSharedConfigProfile(cfg.Auth.Profile),
		)
	case "env":
		// default credential chain handles environment variables
	case "secret":
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.Auth.AccessKeyID, cfg.Auth.SecretAccessKey, ""),
		))
	default:
		return aws.Config{}, fmt.Errorf("unsupported auth.mode for store.s3: %s", cfg.Auth.Mode)
	}

	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// objectKey builds the S3 object key using networkId + rangeKey (block-agnostic).
// The block reference in the partition key is intentionally ignored so that
// SET (which knows the block from response) and GET (which may not know the block)
// always produce the same S3 key. This eliminates the need for reverse index lookups.
func (c *S3Connector) objectKey(partitionKey, rangeKey string) string {
	netId := extractNetworkId(partitionKey)
	key := netId + "/" + rangeKey
	if c.keyPrefix != "" {
		key = c.keyPrefix + key
	}
	return key
}

// extractNetworkId strips the blockRef (last segment after ':') from a partition key.
// "evm:1:18000000" → "evm:1", "evm:137:*" → "evm:137", "evm:1:0xabc" → "evm:1"
func extractNetworkId(partitionKey string) string {
	lastColon := strings.LastIndex(partitionKey, ":")
	if lastColon > 0 {
		return partitionKey[:lastColon]
	}
	return partitionKey
}

func (c *S3Connector) Id() string {
	return c.id
}

func (c *S3Connector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration, metadata interface{}) error {
	ctx, span := common.StartSpan(ctx, "S3Connector.Set")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
			attribute.Int("value_size", len(value)),
		)
	}

	if c.writeClient == nil {
		err := fmt.Errorf("S3 client not initialized yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	c.logger.Debug().
		Int("len", len(value)).
		Str("partitionKey", partitionKey).
		Str("rangeKey", rangeKey).
		Interface("ttl", ttl).
		Msg("putting object in s3")

	ctx, cancel := context.WithTimeout(ctx, c.setTimeout)
	defer cancel()

	objMeta := map[string]string{}
	var expires *time.Time
	if ttl != nil && *ttl > 0 {
		expiresAt := time.Now().Add(*ttl)
		objMeta[s3MetaExpiresAt] = strconv.FormatInt(expiresAt.Unix(), 10)
		expires = &expiresAt
	}

	input := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(c.objectKey(partitionKey, rangeKey)),
		Body:     bytes.NewReader(value),
		Metadata: objMeta,
	}
	if expires != nil {
		input.Expires = expires
	}

	// Apply per-finality storage class if configured
	if c.evmFinalityMetadata != nil {
		if finality, ok := metadata.(common.DataFinalityState); ok {
			var meta *common.S3ObjectMetadata
			switch finality {
			case common.DataFinalityStateFinalized:
				meta = c.evmFinalityMetadata.Finalized
			case common.DataFinalityStateUnfinalized:
				meta = c.evmFinalityMetadata.Unfinalized
			case common.DataFinalityStateRealtime:
				meta = c.evmFinalityMetadata.Realtime
			case common.DataFinalityStateUnknown:
				meta = c.evmFinalityMetadata.Unknown
			}
			if meta != nil && meta.StorageClass != "" {
				input.StorageClass = types.StorageClass(meta.StorageClass)
			}
		}
	}

	_, err := c.writeClient.PutObject(ctx, input)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	return nil
}

func (c *S3Connector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	ctx, span := common.StartSpan(ctx, "S3Connector.Get")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	if c.readClient == nil {
		err := fmt.Errorf("S3 client not initialized yet")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.getTimeout)
	defer cancel()

	// index parameter is ignored — all lookups use the same block-agnostic key.
	// This means both ConnectorMainIndex and ConnectorReverseIndex resolve identically,
	// eliminating the need for reverse index objects entirely.
	objKey := c.objectKey(partitionKey, rangeKey)
	c.logger.Debug().Str("key", objKey).Msg("getting object from s3")

	result, err := c.readClient.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			nfErr := common.NewErrRecordNotFound(partitionKey, rangeKey, S3DriverName)
			common.SetTraceSpanError(span, nfErr)
			return nil, nfErr
		}
		common.SetTraceSpanError(span, err)
		return nil, err
	}
	defer result.Body.Close()

	if expired, expiresAt := isS3Expired(result.Metadata); expired {
		expErr := common.NewErrRecordExpired(partitionKey, rangeKey, S3DriverName, time.Now().Unix(), expiresAt)
		common.SetTraceSpanError(span, expErr)
		return nil, expErr
	}

	value, err := io.ReadAll(result.Body)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	if common.IsTracingDetailed {
		span.SetAttributes(attribute.Int("value_size", len(value)))
	}

	return value, nil
}

func (c *S3Connector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	ctx, span := common.StartSpan(ctx, "S3Connector.Delete")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	if c.writeClient == nil {
		err := fmt.Errorf("S3 client not initialized yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.setTimeout)
	defer cancel()

	_, err := c.writeClient.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.objectKey(partitionKey, rangeKey)),
	})
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	return nil
}

// List is not supported for S3 connector. Use Redis, PostgreSQL, or DynamoDB for listing.
func (c *S3Connector) List(_ context.Context, _ string, _ int, _ string) ([]KeyValuePair, string, error) {
	return nil, "", fmt.Errorf("S3 connector does not support List; use Redis, PostgreSQL, or DynamoDB")
}

// Lock is not supported for S3 connector. Use Redis, PostgreSQL, or DynamoDB for shared state.
func (c *S3Connector) Lock(_ context.Context, _ string, _ time.Duration) (DistributedLock, error) {
	return nil, fmt.Errorf("S3 connector does not support distributed locking; use Redis, PostgreSQL, or DynamoDB for shared state")
}

// WatchCounterInt64 is not supported for S3 connector. Use Redis, PostgreSQL, or DynamoDB for shared state.
func (c *S3Connector) WatchCounterInt64(_ context.Context, _ string) (<-chan CounterInt64State, func(), error) {
	return nil, nil, fmt.Errorf("S3 connector does not support counter watching; use Redis, PostgreSQL, or DynamoDB for shared state")
}

// PublishCounterInt64 is not supported for S3 connector. Use Redis, PostgreSQL, or DynamoDB for shared state.
func (c *S3Connector) PublishCounterInt64(_ context.Context, _ string, _ CounterInt64State) error {
	return fmt.Errorf("S3 connector does not support counter publishing; use Redis, PostgreSQL, or DynamoDB for shared state")
}

// isS3Expired checks if an S3 object has expired based on its metadata.
// The metadata key lookup is case-insensitive because AWS SDK and S3-compatible
// stores (MinIO, LocalStack) may return metadata keys in different casings.
func isS3Expired(metadata map[string]string) (bool, int64) {
	var expiresAtStr string
	for k, v := range metadata {
		if strings.EqualFold(k, s3MetaExpiresAt) {
			expiresAtStr = v
			break
		}
	}
	if expiresAtStr == "" {
		return false, 0
	}
	expiresAt, err := strconv.ParseInt(expiresAtStr, 10, 64)
	if err != nil {
		return false, 0
	}
	now := time.Now().Unix()
	return now > expiresAt, expiresAt
}

func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == 404
	}
	return false
}
