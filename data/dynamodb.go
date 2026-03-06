package data

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http2"
)

const (
	DynamoDBDriverName = "dynamodb"
)

var _ Connector = (*DynamoDBConnector)(nil)

type DynamoDBConnector struct {
	id                string
	logger            *zerolog.Logger
	initializer       *util.Initializer
	writeClient       *dynamodb.Client
	readClient        *dynamodb.Client
	table             string
	ttlAttributeName  string
	partitionKeyName  string
	rangeKeyName      string
	reverseIndexName  string
	initTimeout       time.Duration
	getTimeout        time.Duration
	setTimeout        time.Duration
	statePollInterval time.Duration
	lockRetryInterval time.Duration
}

var _ DistributedLock = &dynamoLock{}

type dynamoLock struct {
	connector *DynamoDBConnector
	lockKey   string
}

func (l *dynamoLock) IsNil() bool {
	return l == nil || l.connector == nil
}

// dynamoPaginationToken is used to serialize/deserialize DynamoDB pagination tokens.
type dynamoPaginationToken struct {
	PK string `json:"pk"`
	RK string `json:"rk"`
}

var sharedReadClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        2048,
		MaxIdleConnsPerHost: 2048,
		MaxConnsPerHost:     0, // unlimited, let MaxIdle… guard memory
		IdleConnTimeout:     120 * time.Second,
	},
}
var sharedWriteClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		MaxConnsPerHost:     0, // unlimited, let MaxIdle… guard memory
		IdleConnTimeout:     120 * time.Second,
	},
}

func init() {
	_ = http2.ConfigureTransport(sharedReadClient.Transport.(*http.Transport))
	_ = http2.ConfigureTransport(sharedWriteClient.Transport.(*http.Transport))
}

func NewDynamoDBConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.DynamoDBConnectorConfig,
) (*DynamoDBConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating dynamodb connector")

	connector := &DynamoDBConnector{
		id:                id,
		logger:            &lg,
		table:             cfg.Table,
		partitionKeyName:  cfg.PartitionKeyName,
		rangeKeyName:      cfg.RangeKeyName,
		reverseIndexName:  cfg.ReverseIndexName,
		ttlAttributeName:  cfg.TTLAttributeName,
		initTimeout:       cfg.InitTimeout.Duration(),
		getTimeout:        cfg.GetTimeout.Duration(),
		setTimeout:        cfg.SetTimeout.Duration(),
		statePollInterval: cfg.StatePollInterval.Duration(),
		lockRetryInterval: cfg.LockRetryInterval.Duration(),
	}

	// create an Initializer to handle (re)connecting
	connector.initializer = util.NewInitializer(ctx, &lg, nil)

	connectTask := util.NewBootstrapTask(fmt.Sprintf("dynamodb-connect/%s", id), func(ctx context.Context) error {
		return connector.connectTask(ctx, cfg)
	})

	if err := connector.initializer.ExecuteTasks(ctx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize DynamoDB on first attempt (will retry in background)")
		// Return the connector so the app can proceed, but note that it's not ready yet.
		return connector, nil
	}

	return connector, nil
}

func (d *DynamoDBConnector) connectTask(ctx context.Context, cfg *common.DynamoDBConnectorConfig) error {
	awsCfg, err := createDynamoDBConfig(ctx, cfg)
	if err != nil {
		return common.NewTaskFatal(err)
	}

	var opts []func(*dynamodb.Options)
	if cfg.Endpoint != "" {
		opts = append(opts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	opts = append(opts, func(o *dynamodb.Options) {
		o.RetryMaxAttempts = cfg.MaxRetries
	})

	writeOpts := make([]func(*dynamodb.Options), len(opts)+1)
	copy(writeOpts, opts)
	writeOpts[len(opts)] = func(o *dynamodb.Options) { o.HTTPClient = sharedWriteClient }
	d.writeClient = dynamodb.NewFromConfig(awsCfg, writeOpts...)

	readOpts := make([]func(*dynamodb.Options), len(opts)+1)
	copy(readOpts, opts)
	readOpts[len(opts)] = func(o *dynamodb.Options) { o.HTTPClient = sharedReadClient }
	d.readClient = dynamodb.NewFromConfig(awsCfg, readOpts...)

	if cfg.Table == "" {
		return common.NewTaskFatal(fmt.Errorf("missing table name for dynamodb connector"))
	}

	err = createTableIfNotExists(ctx, d.logger, d.writeClient, cfg)
	if err != nil {
		return err
	}

	err = ensureGlobalSecondaryIndexes(ctx, d.logger, d.writeClient, cfg)
	if err != nil {
		return err
	}

	return nil
}

func createDynamoDBConfig(ctx context.Context, cfg *common.DynamoDBConnectorConfig) (aws.Config, error) {
	if cfg == nil || cfg.Region == "" {
		return aws.Config{}, fmt.Errorf("missing region for store.dynamodb")
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
		return aws.Config{}, fmt.Errorf("unsupported auth.mode for store.dynamodb: %s", cfg.Auth.Mode)
	}

	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

func createTableIfNotExists(
	ctx context.Context,
	logger *zerolog.Logger,
	client *dynamodb.Client,
	cfg *common.DynamoDBConnectorConfig,
) error {
	logger.Debug().Msgf("creating dynamodb table '%s' if not exists with partition key '%s' and range key %s", cfg.Table, cfg.PartitionKeyName, cfg.RangeKeyName)
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(cfg.Table),
		KeySchema: []types.KeySchemaElement{
			{
				AttributeName: aws.String(cfg.PartitionKeyName),
				KeyType:       types.KeyTypeHash,
			},
			{
				AttributeName: aws.String(cfg.RangeKeyName),
				KeyType:       types.KeyTypeRange,
			},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String(cfg.PartitionKeyName),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String(cfg.RangeKeyName),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		BillingMode: types.BillingModePayPerRequest,
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String(cfg.ReverseIndexName),
				KeySchema: []types.KeySchemaElement{
					{
						AttributeName: aws.String(cfg.RangeKeyName),
						KeyType:       types.KeyTypeHash,
					},
					{
						AttributeName: aws.String(cfg.PartitionKeyName),
						KeyType:       types.KeyTypeRange,
					},
				},
				Projection: &types.Projection{
					ProjectionType: types.ProjectionTypeAll,
				},
			},
		},
	})

	var riue *types.ResourceInUseException
	if err != nil && !errors.As(err, &riue) {
		logger.Error().Err(err).Msgf("failed to create dynamodb table %s", cfg.Table)
		return err
	}

	waiter := dynamodb.NewTableExistsWaiter(client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(cfg.Table),
	}, 5*time.Minute); err != nil {
		logger.Error().Err(err).Msgf("failed to wait for dynamodb table %s to be created", cfg.Table)
		return err
	}

	_, err = client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(cfg.Table),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			AttributeName: aws.String(cfg.TTLAttributeName),
			Enabled:       aws.Bool(true),
		},
	})

	if err != nil && !strings.Contains(err.Error(), "already enabled") {
		logger.Error().Err(err).Msg("failed to enable TTL on table")
		return err
	}

	logger.Debug().Msgf("dynamodb table '%s' is ready", cfg.Table)

	return nil
}

func ensureGlobalSecondaryIndexes(
	ctx context.Context,
	logger *zerolog.Logger,
	client *dynamodb.Client,
	cfg *common.DynamoDBConnectorConfig,
) error {
	if cfg.ReverseIndexName == "" {
		return nil
	}

	logger.Debug().Msgf("ensuring global secondary index '%s' for table '%s'", cfg.ReverseIndexName, cfg.Table)

	currentTable, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(cfg.Table),
	})

	if err != nil {
		return err
	}

	for _, gsi := range currentTable.Table.GlobalSecondaryIndexes {
		if *gsi.IndexName == cfg.ReverseIndexName {
			logger.Debug().Msgf("global secondary index '%s' already exists", cfg.ReverseIndexName)
			return nil
		}
	}

	_, err = client.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String(cfg.Table),
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String(cfg.PartitionKeyName),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String(cfg.RangeKeyName),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{
				Create: &types.CreateGlobalSecondaryIndexAction{
					IndexName: aws.String(cfg.ReverseIndexName),
					KeySchema: []types.KeySchemaElement{
						{
							AttributeName: aws.String(cfg.RangeKeyName),
							KeyType:       types.KeyTypeHash,
						},
						{
							AttributeName: aws.String(cfg.PartitionKeyName),
							KeyType:       types.KeyTypeRange,
						},
					},
					Projection: &types.Projection{
						ProjectionType: types.ProjectionTypeAll,
					},
					// TODO Add capacity unit configuration based on the table's capacity type
				},
			},
		},
	})

	if err != nil {
		logger.Error().Err(err).Msgf("failed to create global secondary index '%s' for table %s", cfg.ReverseIndexName, cfg.Table)
	} else {
		logger.Debug().Msgf("global secondary index '%s' created for table %s", cfg.ReverseIndexName, cfg.Table)
	}

	return err
}

func (d *DynamoDBConnector) Id() string {
	return d.id
}

func (d *DynamoDBConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration, _ interface{}) error {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Set")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
			attribute.Int("value_size", len(value)),
		)
	}

	if d.writeClient == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	if len(value) > 1024 {
		d.logger.Debug().Int("len", len(value)).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Interface("ttl", ttl).Msg("putting item in dynamodb")
	} else {
		d.logger.Debug().Int("len", len(value)).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Interface("ttl", ttl).Msg("putting item in dynamodb")
	}

	item := map[string]types.AttributeValue{
		d.partitionKeyName: &types.AttributeValueMemberS{Value: partitionKey},
		d.rangeKeyName:     &types.AttributeValueMemberS{Value: rangeKey},
		"value":            &types.AttributeValueMemberB{Value: value}, // Using Binary attribute type
	}

	ctx, cancel := context.WithTimeout(ctx, d.setTimeout)
	defer cancel()

	// Add TTL if provided
	if ttl != nil && *ttl > 0 {
		expirationTime := time.Now().Add(*ttl).Unix()
		item[d.ttlAttributeName] = &types.AttributeValueMemberN{
			Value: fmt.Sprintf("%d", expirationTime),
		}
	}

	_, err := d.writeClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item:      item,
	})

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (d *DynamoDBConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Get")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	if d.readClient == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	var value []byte

	if index == ConnectorReverseIndex {
		if rangeKey == "" || strings.HasSuffix(rangeKey, "*") {
			err := fmt.Errorf("when using reverse index rangeKey must be a non-empty string and not contain wildcards (rangeKey: '%s', partitionKey: '%s')", rangeKey, partitionKey)
			common.SetTraceSpanError(span, err)
			return nil, err
		}
		var keyCondition string = "#pkey = :pkey"
		var exprAttrNames map[string]string = map[string]string{
			"#pkey": d.rangeKeyName,
		}
		var exprAttrValues map[string]types.AttributeValue = map[string]types.AttributeValue{
			":pkey": &types.AttributeValueMemberS{Value: rangeKey},
		}

		// In the reverse index, the rangeKey is the HASH key and partitionKey is the RANGE key
		if partitionKey != "" && partitionKey != "*" {
			if strings.HasSuffix(partitionKey, "*") {
				keyCondition += " AND begins_with(#rkey, :rkey)"
			} else {
				keyCondition += " AND #rkey = :rkey"
			}
			exprAttrNames["#rkey"] = d.partitionKeyName
			exprAttrValues[":rkey"] = &types.AttributeValueMemberS{
				Value: strings.TrimSuffix(partitionKey, "*"),
			}
		}

		// Build a query that returns newest first, requesting value and ttl.
		// We fetch up to 10 items and will pick the first non-expired one.
		now := time.Now().Unix()
		qi := &dynamodb.QueryInput{
			TableName:                 aws.String(d.table),
			IndexName:                 aws.String(d.reverseIndexName),
			KeyConditionExpression:    aws.String(keyCondition),
			ExpressionAttributeNames:  exprAttrNames,
			ExpressionAttributeValues: exprAttrValues,
			ScanIndexForward:          aws.Bool(false), // newest first
			Limit:                     aws.Int32(10),   // examine a handful to avoid pagination
			ProjectionExpression:      aws.String("#val, #ttl"),
			Select:                    types.SelectSpecificAttributes,
			FilterExpression:          aws.String("attribute_not_exists(#ttl) OR #ttl = :zero OR #ttl > :now"),
		}
		// Add aliases required by Projection/Filter
		qi.ExpressionAttributeNames["#val"] = "value"
		qi.ExpressionAttributeNames["#ttl"] = d.ttlAttributeName
		qi.ExpressionAttributeValues[":now"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)}
		qi.ExpressionAttributeValues[":zero"] = &types.AttributeValueMemberN{Value: "0"}

		ctx, cancel := context.WithTimeout(ctx, d.getTimeout)
		defer cancel()

		d.logger.Debug().Str("index", d.reverseIndexName).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("getting item from dynamodb")
		result, err := d.readClient.Query(ctx, qi)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return nil, err
		}
		// Find the first non-expired item from the page (FilterExpression already helps)
		var chosen map[string]types.AttributeValue
		for _, it := range result.Items {
			// Extra safeguard: client-side TTL validation
			if nVal, ok := it[d.ttlAttributeName].(*types.AttributeValueMemberN); ok && nVal.Value != "" && nVal.Value != "0" {
				expirationTime, perr := strconv.ParseInt(nVal.Value, 10, 64)
				if perr == nil && now > expirationTime {
					continue // skip expired
				}
			}
			chosen = it
			break
		}

		if chosen == nil {
			err := common.NewErrRecordNotFound(partitionKey, rangeKey, DynamoDBDriverName)
			common.SetTraceSpanError(span, err)
			return nil, err
		}

		// Backward compatibility: check both B and S attributes
		if bVal, ok := chosen["value"].(*types.AttributeValueMemberB); ok {
			value = bVal.Value
		} else if sVal, ok := chosen["value"].(*types.AttributeValueMemberS); ok {
			// Legacy string value - treat as final decompressed value
			value = []byte(sVal.Value)
		} else {
			return nil, fmt.Errorf("value attribute is neither binary nor string")
		}
	} else {
		ky := map[string]types.AttributeValue{
			d.partitionKeyName: &types.AttributeValueMemberS{Value: partitionKey},
			d.rangeKeyName:     &types.AttributeValueMemberS{Value: rangeKey},
		}
		d.logger.Debug().Str("index", "n/a").Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("getting item from dynamodb")
		result, err := d.readClient.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(d.table),
			Key:       ky,
		})

		if err != nil {
			common.SetTraceSpanError(span, err)
			return nil, err
		}

		if result.Item == nil {
			err := common.NewErrRecordNotFound(partitionKey, rangeKey, DynamoDBDriverName)
			common.SetTraceSpanError(span, err)
			return nil, err
		}

		// Check if the item has expired
		if nVal, ok := result.Item[d.ttlAttributeName].(*types.AttributeValueMemberN); ok && nVal.Value != "" && nVal.Value != "0" {
			expirationTime, err := strconv.ParseInt(nVal.Value, 10, 64)
			now := time.Now().Unix()
			if err == nil && now > expirationTime {
				err := common.NewErrRecordExpired(partitionKey, rangeKey, DynamoDBDriverName, now, expirationTime)
				common.SetTraceSpanError(span, err)
				return nil, err
			}
		}

		// Backward compatibility: check both B and S attributes
		if bVal, ok := result.Item["value"].(*types.AttributeValueMemberB); ok {
			value = bVal.Value
		} else if sVal, ok := result.Item["value"].(*types.AttributeValueMemberS); ok {
			// Legacy string value - treat as final decompressed value
			value = []byte(sVal.Value)
		} else {
			return nil, fmt.Errorf("value attribute is neither binary nor string")
		}
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int("value_size", len(value)),
		)
	}

	return value, nil
}

func (d *DynamoDBConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Lock",
		trace.WithAttributes(
			attribute.String("lock_key", key),
			attribute.Int64("ttl_ms", ttl.Milliseconds()),
		),
	)
	defer span.End()

	if d.writeClient == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	lockKey := fmt.Sprintf("%s:lock", key)

	// The 'ttl' parameter defines how long the lock will be held if acquired.
	// The overall acquisition timeout is governed by the parent 'ctx'.

	for {
		// Check if the parent context is already done (e.g., request cancelled or timed out)
		// This check is done at the beginning of each attempt and before waiting for retry.
		select {
		case <-ctx.Done():
			err := fmt.Errorf("lock acquisition cancelled or timed out for key '%s': %w", key, ctx.Err())
			common.SetTraceSpanError(span, err)
			return nil, err
		default:
			// Continue with lock acquisition attempt
		}

		// Calculate expiry time for the lock item itself IF it's acquired in this attempt.
		// This 'ttl' is the duration the lock will be held.
		lockItemExpiryTime := time.Now().Add(ttl).Unix()

		// Context for the individual PutItem attempt.
		// This is bounded by the connector's configured SetTimeout and the parent context 'ctx'.
		// If 'ctx' finishes, PutItem will be interrupted.
		putAttemptCtx, putAttemptCancel := context.WithTimeout(ctx, d.setTimeout)

		_, err := d.writeClient.PutItem(putAttemptCtx, &dynamodb.PutItemInput{
			TableName: aws.String(d.table),
			Item: map[string]types.AttributeValue{
				d.partitionKeyName: &types.AttributeValueMemberS{Value: lockKey},
				d.rangeKeyName:     &types.AttributeValueMemberS{Value: "lock"}, // Using a fixed value for the range key of lock items
				"expiry":           &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", lockItemExpiryTime)},
			},
			ConditionExpression: aws.String(
				"attribute_not_exists(#pk) OR #expiry < :now",
			),
			ExpressionAttributeNames: map[string]string{
				"#pk":     d.partitionKeyName,
				"#expiry": "expiry",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":now": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().Unix())},
			},
		})
		putAttemptCancel() // Release resources for this attempt's context immediately

		if err == nil {
			// Lock acquired successfully
			d.logger.Debug().Str("lockKey", lockKey).Dur("ttl", ttl).Msg("distributed lock acquired")
			return &dynamoLock{
				connector: d,
				lockKey:   lockKey,
			}, nil
		}

		// If an error occurred, check if it's a known AWS error we should retry on
		retryableError := false

		var ccfe *types.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			d.logger.Debug().Str("lockKey", lockKey).Dur("retryInterval", d.lockRetryInterval).
				Msg("lock currently held or contention, will retry")
			retryableError = true
		}

		var pte *types.ProvisionedThroughputExceededException
		var ise *types.InternalServerError
		var lee *types.LimitExceededException
		if errors.As(err, &pte) || errors.As(err, &ise) || errors.As(err, &lee) {
			d.logger.Warn().Err(err).Str("lockKey", lockKey).
				Dur("retryInterval", d.lockRetryInterval).
				Msg("DynamoDB capacity/limit/server error during lock acquisition, will retry")
			retryableError = true
		}

		if !retryableError {
			// Non-retryable error for lock acquisition logic
			d.logger.Error().Err(err).Str("lockKey", lockKey).
				Msg("non-retryable error during lock acquisition attempt")
		}

		if retryableError {
			// Wait for d.lockRetryInterval before retrying, but also respect parent context cancellation.
			select {
			case <-time.After(d.lockRetryInterval):
				// Continue to the next iteration of the loop to retry
			case <-ctx.Done(): // Parent context was cancelled/timed out while waiting
				wrappedErr := fmt.Errorf("lock acquisition timed out while waiting to retry for key '%s': %w", key, ctx.Err())
				common.SetTraceSpanError(span, wrappedErr)
				return nil, wrappedErr
			}
		} else {
			// An unexpected or non-retryable error occurred during the PutItem attempt
			wrappedErr := fmt.Errorf("failed to acquire lock for key '%s' during attempt: %w", key, err)
			common.SetTraceSpanError(span, wrappedErr)
			return nil, wrappedErr
		}
		// If a retryableError occurred and context wasn't done, the loop continues.
	}
}

func (l *dynamoLock) Unlock(ctx context.Context) error {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Unlock",
		trace.WithAttributes(
			attribute.String("lock_key", l.lockKey),
		),
	)
	defer span.End()

	_, err := l.connector.writeClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(l.connector.table),
		Key: map[string]types.AttributeValue{
			l.connector.partitionKeyName: &types.AttributeValueMemberS{Value: l.lockKey},
			l.connector.rangeKeyName:     &types.AttributeValueMemberS{Value: "lock"},
		},
	})

	if err != nil {
		l.connector.logger.Warn().Err(err).Str("lockKey", l.lockKey).Msg("failed to release distributed lock")
	} else {
		l.connector.logger.Debug().Str("lockKey", l.lockKey).Msg("distributed lock released")
	}

	return err
}

func (d *DynamoDBConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	if d.readClient == nil {
		return nil, nil, fmt.Errorf("DynamoDB client not initialized yet")
	}

	updates := make(chan CounterInt64State, 1)

	// Start polling goroutine
	ticker := time.NewTicker(d.statePollInterval)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		var lastUpdatedAt int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				st, ok, err := d.getSimpleValue(ctx, key)
				if err != nil {
					d.logger.Warn().Err(err).Str("key", key).Msg("failed to poll counter value")
					continue
				}
				if ok && st.UpdatedAt > lastUpdatedAt {
					lastUpdatedAt = st.UpdatedAt
					select {
					case updates <- st:
					default:
					}
				}
			}
		}
	}()

	// Get initial value
	if st, ok, err := d.getSimpleValue(ctx, key); err == nil && ok {
		updates <- st
	}

	cleanup := func() {
		close(done)
		close(updates)
	}

	return updates, cleanup, nil
}

func (d *DynamoDBConnector) getSimpleValue(ctx context.Context, key string) (CounterInt64State, bool, error) {
	ctx, span := common.StartDetailSpan(ctx, "DynamoDBConnector.getSimpleValue",
		trace.WithAttributes(
			attribute.String("key", key),
		),
	)
	defer span.End()

	result, err := d.readClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table),
		Key: map[string]types.AttributeValue{
			d.partitionKeyName: &types.AttributeValueMemberS{Value: key},
			d.rangeKeyName:     &types.AttributeValueMemberS{Value: "value"},
		},
		ConsistentRead: aws.Bool(true),
	})

	if err != nil {
		return CounterInt64State{}, false, err
	}

	if result.Item == nil {
		return CounterInt64State{}, false, nil
	}

	// Check if value attribute exists
	v, exists := result.Item["value"]
	if !exists {
		d.logger.Debug().Str("key", key).Msg("counter value attribute not found in DynamoDB item")
		return CounterInt64State{}, false, nil
	}

	// Handle nil AttributeValue
	if v == nil {
		d.logger.Debug().Str("key", key).Msg("counter value attribute is nil")
		return CounterInt64State{}, false, nil
	}

	// Extract raw bytes (we store JSON in Binary; other types are treated as best-effort strings)
	var raw []byte
	switch val := v.(type) {
	case *types.AttributeValueMemberB:
		raw = val.Value
	case *types.AttributeValueMemberS:
		raw = []byte(val.Value)
	case *types.AttributeValueMemberN:
		raw = []byte(val.Value)
	default:
		return CounterInt64State{}, false, nil
	}

	var st CounterInt64State
	if err := common.SonicCfg.Unmarshal(raw, &st); err != nil || st.UpdatedAt <= 0 {
		// No backward compatibility: treat parse errors as missing
		return CounterInt64State{}, false, nil
	}

	return st, true, nil
}

func (d *DynamoDBConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	// DynamoDB connector doesn't have a push-based pub/sub mechanism.
	// Shared counters are propagated via polling WatchCounterInt64, and the authoritative state
	// is stored via Set(). Publish is therefore a best-effort no-op.
	return nil
}

func (d *DynamoDBConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Delete")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	if d.writeClient == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	d.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("deleting item from dynamodb")

	ctx, cancel := context.WithTimeout(ctx, d.setTimeout)
	defer cancel()

	_, err := d.writeClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.table),
		Key: map[string]types.AttributeValue{
			d.partitionKeyName: &types.AttributeValueMemberS{Value: partitionKey},
			d.rangeKeyName:     &types.AttributeValueMemberS{Value: rangeKey},
		},
	})

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (d *DynamoDBConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.List")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.Int("limit", limit),
		)
	}

	if d.readClient == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	ctx, cancel := context.WithTimeout(ctx, d.getTimeout)
	defer cancel()

	// Parse pagination token
	var exclusiveStartKey map[string]types.AttributeValue
	if paginationToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(paginationToken)
		if err != nil {
			return nil, "", fmt.Errorf("invalid pagination token: %w", err)
		}
		var tok dynamoPaginationToken
		if err := json.Unmarshal(decoded, &tok); err != nil {
			return nil, "", fmt.Errorf("invalid pagination token format: %w", err)
		}
		exclusiveStartKey = map[string]types.AttributeValue{
			d.partitionKeyName: &types.AttributeValueMemberS{Value: tok.PK},
			d.rangeKeyName:     &types.AttributeValueMemberS{Value: tok.RK},
		}
	}

	var result *dynamodb.ScanOutput
	var err error

	if index == ConnectorReverseIndex {
		// Use the reverse index (GSI)
		result, err = d.readClient.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(d.table),
			IndexName:         aws.String(d.reverseIndexName),
			Limit:             aws.Int32(int32(limit)),
			ExclusiveStartKey: exclusiveStartKey,
		})
	} else {
		// Use the main table
		result, err = d.readClient.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(d.table),
			Limit:             aws.Int32(int32(limit)),
			ExclusiveStartKey: exclusiveStartKey,
		})
	}

	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	results := make([]KeyValuePair, 0, len(result.Items))
	now := time.Now().Unix()

	for _, item := range result.Items {
		// Check if item has expired
		if nVal, ok := item[d.ttlAttributeName].(*types.AttributeValueMemberN); ok && nVal.Value != "" && nVal.Value != "0" {
			expirationTime, err := strconv.ParseInt(nVal.Value, 10, 64)
			if err == nil && now > expirationTime {
				continue // Skip expired items
			}
		}

		// Extract partition and range keys
		partitionKey := ""
		rangeKey := ""
		if sVal, ok := item[d.partitionKeyName].(*types.AttributeValueMemberS); ok {
			partitionKey = sVal.Value
		}
		if sVal, ok := item[d.rangeKeyName].(*types.AttributeValueMemberS); ok {
			rangeKey = sVal.Value
		}

		// Extract value
		var value []byte
		if item["value"] != nil {
			if bVal, ok := item["value"].(*types.AttributeValueMemberB); ok {
				value = bVal.Value
			} else if sVal, ok := item["value"].(*types.AttributeValueMemberS); ok {
				value = []byte(sVal.Value)
			}
		}

		if partitionKey != "" && rangeKey != "" {
			results = append(results, KeyValuePair{
				PartitionKey: partitionKey,
				RangeKey:     rangeKey,
				Value:        value,
			})
		}
	}

	// Prepare next token
	nextToken := ""
	if result.LastEvaluatedKey != nil {
		pk := ""
		rk := ""
		if v, ok := result.LastEvaluatedKey[d.partitionKeyName].(*types.AttributeValueMemberS); ok {
			pk = v.Value
		}
		if v, ok := result.LastEvaluatedKey[d.rangeKeyName].(*types.AttributeValueMemberS); ok {
			rk = v.Value
		}
		tokenData, err := json.Marshal(dynamoPaginationToken{PK: pk, RK: rk})
		if err != nil {
			return nil, "", fmt.Errorf("failed to create pagination token: %w", err)
		}
		nextToken = base64.StdEncoding.EncodeToString(tokenData)
	}

	return results, nextToken, nil
}
