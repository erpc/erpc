package data

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http2"
)

const (
	S3DriverName = "s3"

	// S3 metadata key for TTL tracking (AWS SDK lowercases metadata keys)
	s3MetaExpiresAt = "expires-at"

	// Prefix for reverse index objects
	s3ReverseIndexPrefix = "_rvi/"
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
	id          string
	logger      *zerolog.Logger
	initializer *util.Initializer
	readClient  *s3.S3
	writeClient *s3.S3
	bucket      string
	keyPrefix   string
	initTimeout time.Duration
	getTimeout  time.Duration
	setTimeout  time.Duration
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
		id:          id,
		logger:      &lg,
		bucket:      cfg.Bucket,
		keyPrefix:   cfg.KeyPrefix,
		initTimeout: cfg.InitTimeout.Duration(),
		getTimeout:  cfg.GetTimeout.Duration(),
		setTimeout:  cfg.SetTimeout.Duration(),
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
	sess, err := createS3Session(cfg)
	if err != nil {
		return common.NewTaskFatal(err)
	}

	baseCfg := &aws.Config{
		Region:     aws.String(cfg.Region),
		MaxRetries: aws.Int(cfg.MaxRetries),
	}
	if cfg.Endpoint != "" {
		baseCfg.Endpoint = aws.String(cfg.Endpoint)
		baseCfg.S3ForcePathStyle = aws.Bool(true)
	}

	writeCfg := baseCfg.Copy()
	writeCfg.HTTPClient = s3SharedWriteClient
	c.writeClient = s3.New(sess, writeCfg)

	readCfg := baseCfg.Copy()
	readCfg.HTTPClient = s3SharedReadClient
	c.readClient = s3.New(sess, readCfg)

	_, err = c.readClient.HeadBucketWithContext(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	if err != nil {
		return fmt.Errorf("unable to access S3 bucket %s: %w", c.bucket, err)
	}

	c.logger.Debug().Str("bucket", c.bucket).Msg("s3 bucket is accessible")
	return nil
}

func createS3Session(cfg *common.S3ConnectorConfig) (*session.Session, error) {
	if cfg == nil || cfg.Region == "" {
		return nil, fmt.Errorf("missing region for store.s3")
	}

	awsCfg := &aws.Config{
		Region: aws.String(cfg.Region),
		HTTPClient: &http.Client{
			Timeout: cfg.InitTimeout.Duration(),
		},
	}

	if cfg.Auth == nil {
		return session.NewSession(awsCfg)
	}

	var creds *credentials.Credentials
	switch cfg.Auth.Mode {
	case "file":
		creds = credentials.NewSharedCredentials(cfg.Auth.CredentialsFile, cfg.Auth.Profile)
	case "env":
		creds = credentials.NewEnvCredentials()
	case "secret":
		creds = credentials.NewStaticCredentials(cfg.Auth.AccessKeyID, cfg.Auth.SecretAccessKey, "")
	default:
		return nil, fmt.Errorf("unsupported auth.mode for store.s3: %s", cfg.Auth.Mode)
	}

	awsCfg.Credentials = creds
	return session.NewSession(awsCfg)
}

// objectKey builds the main index S3 object key.
func (c *S3Connector) objectKey(partitionKey, rangeKey string) string {
	key := partitionKey + "/" + rangeKey
	if c.keyPrefix != "" {
		key = c.keyPrefix + key
	}
	return key
}

// reverseIndexKey builds the reverse index S3 object key.
func (c *S3Connector) reverseIndexKey(partitionKey, rangeKey string) string {
	key := s3ReverseIndexPrefix + rangeKey + "/" + partitionKey
	if c.keyPrefix != "" {
		key = c.keyPrefix + key
	}
	return key
}

func (c *S3Connector) Id() string {
	return c.id
}

func (c *S3Connector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
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

	metadata := map[string]*string{}
	var expires *time.Time
	if ttl != nil && *ttl > 0 {
		expiresAt := time.Now().Add(*ttl)
		metadata[s3MetaExpiresAt] = aws.String(strconv.FormatInt(expiresAt.Unix(), 10))
		expires = &expiresAt
	}

	input := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(c.objectKey(partitionKey, rangeKey)),
		Body:     bytes.NewReader(value),
		Metadata: metadata,
	}
	if expires != nil {
		input.Expires = expires
	}

	_, err := c.writeClient.PutObjectWithContext(ctx, input)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	// Write reverse index object (best-effort)
	rviInput := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(c.reverseIndexKey(partitionKey, rangeKey)),
		Body:     bytes.NewReader([]byte(partitionKey)),
		Metadata: metadata,
	}
	if expires != nil {
		rviInput.Expires = expires
	}

	_, rviErr := c.writeClient.PutObjectWithContext(ctx, rviInput)
	if rviErr != nil {
		c.logger.Warn().Err(rviErr).
			Str("partitionKey", partitionKey).
			Str("rangeKey", rangeKey).
			Msg("failed to write reverse index object")
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

	if index == ConnectorReverseIndex {
		return c.getReverseIndex(ctx, span, partitionKey, rangeKey)
	}

	objKey := c.objectKey(partitionKey, rangeKey)
	c.logger.Debug().Str("key", objKey).Msg("getting object from s3")

	result, err := c.readClient.GetObjectWithContext(ctx, &s3.GetObjectInput{
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
		go c.deleteExpiredObjects(c.objectKey(partitionKey, rangeKey), c.reverseIndexKey(partitionKey, rangeKey))
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

func (c *S3Connector) getReverseIndex(ctx context.Context, span trace.Span, partitionKey, rangeKey string) ([]byte, error) {
	if rangeKey == "" || strings.HasSuffix(rangeKey, "*") {
		err := fmt.Errorf("when using reverse index rangeKey must be a non-empty string and not contain wildcards (rangeKey: '%s', partitionKey: '%s')", rangeKey, partitionKey)
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// If partitionKey is exact (no wildcard), do a direct fetch
	if partitionKey != "" && partitionKey != "*" && !strings.HasSuffix(partitionKey, "*") {
		return c.getDirectObject(ctx, span, partitionKey, rangeKey)
	}

	// List objects under _rvi/{rangeKey}/ to find matching partition keys
	prefix := s3ReverseIndexPrefix + rangeKey + "/"
	if c.keyPrefix != "" {
		prefix = c.keyPrefix + prefix
	}

	listInput := &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int64(10),
	}

	c.logger.Debug().Str("prefix", prefix).Msg("listing reverse index in s3")
	result, err := c.readClient.ListObjectsV2WithContext(ctx, listInput)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	for _, obj := range result.Contents {
		objKey := aws.StringValue(obj.Key)

		// Extract partitionKey from the object key: {prefix}_rvi/{rangeKey}/{partitionKey}
		stripped := objKey
		if c.keyPrefix != "" {
			stripped = strings.TrimPrefix(stripped, c.keyPrefix)
		}
		// stripped is now: _rvi/{rangeKey}/{partitionKey}
		parts := strings.SplitN(stripped, "/", 3)
		if len(parts) < 3 {
			continue
		}
		pk := parts[2]

		// Check wildcard prefix match
		if partitionKey != "" && partitionKey != "*" && strings.HasSuffix(partitionKey, "*") {
			trimmed := strings.TrimSuffix(partitionKey, "*")
			if !strings.HasPrefix(pk, trimmed) {
				continue
			}
		}

		// Fetch the actual object
		value, err := c.getDirectObject(ctx, span, pk, rangeKey)
		if err != nil {
			// Stale reverse index or expired — try next
			continue
		}
		return value, nil
	}

	nfErr := common.NewErrRecordNotFound(partitionKey, rangeKey, S3DriverName)
	common.SetTraceSpanError(span, nfErr)
	return nil, nfErr
}

func (c *S3Connector) getDirectObject(ctx context.Context, span trace.Span, partitionKey, rangeKey string) ([]byte, error) {
	objKey := c.objectKey(partitionKey, rangeKey)
	result, err := c.readClient.GetObjectWithContext(ctx, &s3.GetObjectInput{
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
		go c.deleteExpiredObjects(objKey, c.reverseIndexKey(partitionKey, rangeKey))
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

	_, err := c.writeClient.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.objectKey(partitionKey, rangeKey)),
	})
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	// Delete reverse index (best-effort)
	_, rviErr := c.writeClient.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.reverseIndexKey(partitionKey, rangeKey)),
	})
	if rviErr != nil {
		c.logger.Warn().Err(rviErr).Msg("failed to delete reverse index object")
	}

	return nil
}

func (c *S3Connector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	ctx, span := common.StartSpan(ctx, "S3Connector.List")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.Int("limit", limit),
		)
	}

	if c.readClient == nil {
		err := fmt.Errorf("S3 client not initialized yet")
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.getTimeout)
	defer cancel()

	prefix := c.keyPrefix
	if index == ConnectorReverseIndex {
		prefix += s3ReverseIndexPrefix
	}

	results := make([]KeyValuePair, 0, limit)
	var nextToken string
	continuationToken := paginationToken

	// Paginate until we have enough items, because internal prefix objects (_rvi/, etc.) are filtered out
	for len(results) < limit {
		// Request more than needed to account for filtered items
		fetchSize := int64(limit - len(results))
		if index != ConnectorReverseIndex {
			fetchSize *= 3 // over-fetch for main index since _rvi/ objects will be skipped
		}
		if fetchSize < 10 {
			fetchSize = 10
		}

		listInput := &s3.ListObjectsV2Input{
			Bucket:  aws.String(c.bucket),
			Prefix:  aws.String(prefix),
			MaxKeys: aws.Int64(fetchSize),
		}
		if continuationToken != "" {
			listInput.ContinuationToken = aws.String(continuationToken)
		}

		result, err := c.readClient.ListObjectsV2WithContext(ctx, listInput)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return nil, "", err
		}

		for _, obj := range result.Contents {
			if len(results) >= limit {
				break
			}

			objKey := aws.StringValue(obj.Key)
			stripped := objKey
			if c.keyPrefix != "" {
				stripped = strings.TrimPrefix(stripped, c.keyPrefix)
			}

			// Skip internal prefixes for main index listing
			if index != ConnectorReverseIndex && strings.HasPrefix(stripped, s3ReverseIndexPrefix) {
				continue
			}

			// Parse key
			var partitionKey, rangeKey string
			if index == ConnectorReverseIndex {
				trimmed := strings.TrimPrefix(stripped, s3ReverseIndexPrefix)
				parts := strings.SplitN(trimmed, "/", 2)
				if len(parts) != 2 {
					continue
				}
				rangeKey, partitionKey = parts[0], parts[1]
			} else {
				parts := strings.SplitN(stripped, "/", 2)
				if len(parts) != 2 {
					continue
				}
				partitionKey, rangeKey = parts[0], parts[1]
			}

			// Fetch the value and check TTL from the same GetObject response (avoids extra HeadObject round-trip)
			getResult, getErr := c.readClient.GetObjectWithContext(ctx, &s3.GetObjectInput{
				Bucket: aws.String(c.bucket),
				Key:    aws.String(objKey),
			})
			if getErr != nil {
				continue
			}
			if expired, _ := isS3Expired(getResult.Metadata); expired {
				_ = getResult.Body.Close()
				go c.deleteExpiredObjects(objKey, c.reverseIndexKey(partitionKey, rangeKey))
				continue
			}
			value, readErr := io.ReadAll(getResult.Body)
			_ = getResult.Body.Close()
			if readErr != nil {
				continue
			}

			results = append(results, KeyValuePair{
				PartitionKey: partitionKey,
				RangeKey:     rangeKey,
				Value:        value,
			})
		}

		// Check if there are more pages
		if result.NextContinuationToken != nil && *result.IsTruncated {
			continuationToken = aws.StringValue(result.NextContinuationToken)
			nextToken = continuationToken
		} else {
			nextToken = ""
			break
		}
	}

	return results, nextToken, nil
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
func isS3Expired(metadata map[string]*string) (bool, int64) {
	var expiresAtStr *string
	for k, v := range metadata {
		if strings.EqualFold(k, s3MetaExpiresAt) {
			expiresAtStr = v
			break
		}
	}
	if expiresAtStr == nil || *expiresAtStr == "" {
		return false, 0
	}
	expiresAt, err := strconv.ParseInt(*expiresAtStr, 10, 64)
	if err != nil {
		return false, 0
	}
	now := time.Now().Unix()
	return now > expiresAt, expiresAt
}

// deleteExpiredObjects asynchronously deletes expired S3 objects (lazy cleanup).
func (c *S3Connector) deleteExpiredObjects(keys ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, key := range keys {
		_, err := c.writeClient.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(c.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			c.logger.Debug().Err(err).Str("key", key).Msg("failed to delete expired S3 object")
		}
	}
}

func isS3NotFound(err error) bool {
	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case s3.ErrCodeNoSuchKey, "NotFound", "404":
			return true
		}
	}
	if reqErr, ok := err.(awserr.RequestFailure); ok {
		return reqErr.StatusCode() == 404
	}
	return false
}
