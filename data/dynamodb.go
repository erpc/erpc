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

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
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
	writeClient       *dynamodb.DynamoDB
	readClient        *dynamodb.DynamoDB
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
	sess, err := createSession(cfg)
	if err != nil {
		return common.NewTaskFatal(err)
	}

	d.writeClient = dynamodb.New(sess, &aws.Config{
		Endpoint:   aws.String(cfg.Endpoint),
		HTTPClient: sharedWriteClient,
		MaxRetries: aws.Int(cfg.MaxRetries),
		Region:     aws.String(cfg.Region),
	})

	d.readClient = dynamodb.New(sess, &aws.Config{
		Endpoint:   aws.String(cfg.Endpoint),
		HTTPClient: sharedReadClient,
		MaxRetries: aws.Int(cfg.MaxRetries),
		Region:     aws.String(cfg.Region),
	})

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

func createSession(cfg *common.DynamoDBConnectorConfig) (*session.Session, error) {
	if cfg == nil || cfg.Region == "" {
		return nil, fmt.Errorf("missing region for store.dynamodb")
	}

	if cfg.Auth == nil {
		return session.NewSession(&aws.Config{
			Region:   aws.String(cfg.Region),
			Endpoint: aws.String(cfg.Endpoint),
			HTTPClient: &http.Client{
				Timeout: cfg.InitTimeout.Duration(),
			},
		})
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
		return nil, fmt.Errorf("unsupported auth.mode for store.dynamodb: %s", cfg.Auth.Mode)
	}

	return session.NewSession(&aws.Config{
		Region:      aws.String(cfg.Region),
		Credentials: creds,
		HTTPClient: &http.Client{
			Timeout: cfg.InitTimeout.Duration(),
		},
	})
}

func createTableIfNotExists(
	ctx context.Context,
	logger *zerolog.Logger,
	client *dynamodb.DynamoDB,
	cfg *common.DynamoDBConnectorConfig,
) error {
	logger.Debug().Msgf("creating dynamodb table '%s' if not exists with partition key '%s' and range key %s", cfg.Table, cfg.PartitionKeyName, cfg.RangeKeyName)
	_, err := client.CreateTableWithContext(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(cfg.Table),
		KeySchema: []*dynamodb.KeySchemaElement{
			{
				AttributeName: aws.String(cfg.PartitionKeyName),
				KeyType:       aws.String("HASH"),
			},
			{
				AttributeName: aws.String(cfg.RangeKeyName),
				KeyType:       aws.String("RANGE"),
			},
		},
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			{
				AttributeName: aws.String(cfg.PartitionKeyName),
				AttributeType: aws.String("S"),
			},
			{
				AttributeName: aws.String(cfg.RangeKeyName),
				AttributeType: aws.String("S"),
			},
		},
		BillingMode: aws.String("PAY_PER_REQUEST"),
		GlobalSecondaryIndexes: []*dynamodb.GlobalSecondaryIndex{
			{
				IndexName: aws.String(cfg.ReverseIndexName),
				KeySchema: []*dynamodb.KeySchemaElement{
					{
						AttributeName: aws.String(cfg.RangeKeyName),
						KeyType:       aws.String("HASH"),
					},
					{
						AttributeName: aws.String(cfg.PartitionKeyName),
						KeyType:       aws.String("RANGE"),
					},
				},
				Projection: &dynamodb.Projection{
					ProjectionType: aws.String("ALL"),
				},
			},
		},
	})

	if err != nil && !strings.Contains(err.Error(), dynamodb.ErrCodeResourceInUseException) {
		logger.Error().Err(err).Msgf("failed to create dynamodb table %s", cfg.Table)
		return err
	}

	if err := client.WaitUntilTableExistsWithContext(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(cfg.Table),
	}); err != nil {
		logger.Error().Err(err).Msgf("failed to wait for dynamodb table %s to be created", cfg.Table)
		return err
	}

	_, err = client.UpdateTimeToLive(&dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(cfg.Table),
		TimeToLiveSpecification: &dynamodb.TimeToLiveSpecification{
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
	client *dynamodb.DynamoDB,
	cfg *common.DynamoDBConnectorConfig,
) error {
	if cfg.ReverseIndexName == "" {
		return nil
	}

	logger.Debug().Msgf("ensuring global secondary index '%s' for table '%s'", cfg.ReverseIndexName, cfg.Table)

	currentTable, err := client.DescribeTableWithContext(ctx, &dynamodb.DescribeTableInput{
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

	_, err = client.UpdateTableWithContext(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String(cfg.Table),
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			{
				AttributeName: aws.String(cfg.PartitionKeyName),
				AttributeType: aws.String("S"),
			},
			{
				AttributeName: aws.String(cfg.RangeKeyName),
				AttributeType: aws.String("S"),
			},
		},
		GlobalSecondaryIndexUpdates: []*dynamodb.GlobalSecondaryIndexUpdate{
			{
				Create: &dynamodb.CreateGlobalSecondaryIndexAction{
					IndexName: aws.String(cfg.ReverseIndexName),
					KeySchema: []*dynamodb.KeySchemaElement{
						{
							AttributeName: aws.String(cfg.RangeKeyName),
							KeyType:       aws.String("HASH"),
						},
						{
							AttributeName: aws.String(cfg.PartitionKeyName),
							KeyType:       aws.String("RANGE"),
						},
					},
					Projection: &dynamodb.Projection{
						ProjectionType: aws.String("ALL"),
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

func (d *DynamoDBConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
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

	item := map[string]*dynamodb.AttributeValue{
		d.partitionKeyName: {
			S: aws.String(partitionKey),
		},
		d.rangeKeyName: {
			S: aws.String(rangeKey),
		},
		"value": {
			B: value, // Using Binary attribute type
		},
	}

	ctx, cancel := context.WithTimeout(ctx, d.setTimeout)
	defer cancel()

	// Add TTL if provided
	if ttl != nil && *ttl > 0 {
		expirationTime := time.Now().Add(*ttl).Unix()
		item[d.ttlAttributeName] = &dynamodb.AttributeValue{
			N: aws.String(fmt.Sprintf("%d", expirationTime)),
		}
	}

	_, err := d.writeClient.PutItemWithContext(ctx, &dynamodb.PutItemInput{
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
		var exprAttrNames map[string]*string = map[string]*string{
			"#pkey": aws.String(d.rangeKeyName),
		}
		var exprAttrValues map[string]*dynamodb.AttributeValue = map[string]*dynamodb.AttributeValue{
			":pkey": {
				S: aws.String(rangeKey),
			},
		}

		// In the reverse index, the rangeKey is the HASH key and partitionKey is the RANGE key
		if partitionKey != "" && partitionKey != "*" {
			if strings.HasSuffix(partitionKey, "*") {
				keyCondition += " AND begins_with(#rkey, :rkey)"
			} else {
				keyCondition += " AND #rkey = :rkey"
			}
			exprAttrNames["#rkey"] = aws.String(d.partitionKeyName)
			exprAttrValues[":rkey"] = &dynamodb.AttributeValue{
				S: aws.String(strings.TrimSuffix(partitionKey, "*")),
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
			Limit:                     aws.Int64(10),   // examine a handful to avoid pagination
			ProjectionExpression:      aws.String("#val, #ttl"),
			Select:                    aws.String("SPECIFIC_ATTRIBUTES"),
			FilterExpression:          aws.String("attribute_not_exists(#ttl) OR #ttl = :zero OR #ttl > :now"),
		}
		// Add aliases required by Projection/Filter
		qi.ExpressionAttributeNames["#val"] = aws.String("value")
		qi.ExpressionAttributeNames["#ttl"] = aws.String(d.ttlAttributeName)
		qi.ExpressionAttributeValues[":now"] = &dynamodb.AttributeValue{N: aws.String(strconv.FormatInt(now, 10))}
		qi.ExpressionAttributeValues[":zero"] = &dynamodb.AttributeValue{N: aws.String("0")}

		ctx, cancel := context.WithTimeout(ctx, d.getTimeout)
		defer cancel()

		d.logger.Debug().Str("index", d.reverseIndexName).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("getting item from dynamodb")
		result, err := d.readClient.QueryWithContext(ctx, qi, request.WithResponseReadTimeout(d.getTimeout))
		if err != nil {
			common.SetTraceSpanError(span, err)
			return nil, err
		}
		// Find the first non-expired item from the page (FilterExpression already helps)
		var chosen map[string]*dynamodb.AttributeValue
		for _, it := range result.Items {
			// Extra safeguard: client-side TTL validation
			if ttl, exists := it[d.ttlAttributeName]; exists && ttl.N != nil && *ttl.N != "" && *ttl.N != "0" {
				expirationTime, perr := strconv.ParseInt(*ttl.N, 10, 64)
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
		if chosen["value"].B != nil {
			value = chosen["value"].B
		} else if chosen["value"].S != nil {
			// Legacy string value - treat as final decompressed value
			value = []byte(*chosen["value"].S)
		} else {
			return nil, fmt.Errorf("value attribute is neither binary nor string")
		}
	} else {
		ky := map[string]*dynamodb.AttributeValue{
			d.partitionKeyName: {
				S: aws.String(partitionKey),
			},
			d.rangeKeyName: {
				S: aws.String(rangeKey),
			},
		}
		d.logger.Debug().Str("index", "n/a").Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("getting item from dynamodb")
		result, err := d.readClient.GetItemWithContext(ctx, &dynamodb.GetItemInput{
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
		if ttl, exists := result.Item[d.ttlAttributeName]; exists && ttl.N != nil && *ttl.N != "" && *ttl.N != "0" {
			expirationTime, err := strconv.ParseInt(*ttl.N, 10, 64)
			now := time.Now().Unix()
			if err == nil && now > expirationTime {
				err := common.NewErrRecordExpired(partitionKey, rangeKey, DynamoDBDriverName, now, expirationTime)
				common.SetTraceSpanError(span, err)
				return nil, err
			}
		}

		// Backward compatibility: check both B and S attributes
		if result.Item["value"].B != nil {
			value = result.Item["value"].B
		} else if result.Item["value"].S != nil {
			// Legacy string value - treat as final decompressed value
			value = []byte(*result.Item["value"].S)
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

	// Track whether the most recent failure was due to contention (lock held by another caller)
	// vs infrastructure errors. Used to return the correct sentinel error on context timeout.
	lastFailWasContention := false

	for {
		// Check if the parent context is already done (e.g., request cancelled or timed out)
		// This check is done at the beginning of each attempt and before waiting for retry.
		select {
		case <-ctx.Done():
			if lastFailWasContention {
				err := fmt.Errorf("lock acquisition timed out for key '%s' (lock held by another instance): %w", key, errors.Join(ErrLockContention, ctx.Err()))
				common.SetTraceSpanError(span, err)
				return nil, err
			}
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
		// If 'ctx' finishes, PutItemWithContext will be interrupted.
		putAttemptCtx, putAttemptCancel := context.WithTimeout(ctx, d.setTimeout)

		_, err := d.writeClient.PutItemWithContext(putAttemptCtx, &dynamodb.PutItemInput{
			TableName: aws.String(d.table),
			Item: map[string]*dynamodb.AttributeValue{
				d.partitionKeyName: {S: aws.String(lockKey)},
				d.rangeKeyName:     {S: aws.String("lock")}, // Using a fixed value for the range key of lock items
				"expiry":           {N: aws.String(fmt.Sprintf("%d", lockItemExpiryTime))},
			},
			ConditionExpression: aws.String(
				"attribute_not_exists(#pk) OR #expiry < :now",
			),
			ExpressionAttributeNames: map[string]*string{
				"#pk":     aws.String(d.partitionKeyName),
				"#expiry": aws.String("expiry"),
			},
			ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
				":now": {N: aws.String(fmt.Sprintf("%d", time.Now().Unix()))},
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
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case dynamodb.ErrCodeConditionalCheckFailedException:
				d.logger.Debug().Str("lockKey", lockKey).Dur("retryInterval", d.lockRetryInterval).
					Msg("lock currently held or contention, will retry")
				retryableError = true
				lastFailWasContention = true
			case dynamodb.ErrCodeProvisionedThroughputExceededException,
				dynamodb.ErrCodeInternalServerError,    // Often retryable for DynamoDB
				dynamodb.ErrCodeLimitExceededException: // Covers various limits, potentially including some throttling
				d.logger.Warn().Err(aerr).Str("lockKey", lockKey).Str("errorCode", aerr.Code()).
					Dur("retryInterval", d.lockRetryInterval).
					Msg("DynamoDB capacity/limit/server error during lock acquisition, will retry")
				retryableError = true
				lastFailWasContention = false
			default:
				// Non-retryable AWS error for lock acquisition logic
				d.logger.Error().Err(aerr).Str("lockKey", lockKey).Str("errorCode", aerr.Code()).
					Msg("non-retryable AWS error during lock acquisition attempt")
			}
		}

		if retryableError {
			// Wait for d.lockRetryInterval before retrying, but also respect parent context cancellation.
			select {
			case <-time.After(d.lockRetryInterval):
				// Continue to the next iteration of the loop to retry
			case <-ctx.Done(): // Parent context was cancelled/timed out while waiting
				if lastFailWasContention {
					wrappedErr := fmt.Errorf("lock acquisition timed out for key '%s' (lock held by another instance): %w", key, errors.Join(ErrLockContention, ctx.Err()))
					common.SetTraceSpanError(span, wrappedErr)
					return nil, wrappedErr
				}
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

	_, err := l.connector.writeClient.DeleteItemWithContext(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(l.connector.table),
		Key: map[string]*dynamodb.AttributeValue{
			l.connector.partitionKeyName: {S: aws.String(l.lockKey)},
			l.connector.rangeKeyName:     {S: aws.String("lock")},
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

	result, err := d.readClient.GetItemWithContext(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table),
		Key: map[string]*dynamodb.AttributeValue{
			d.partitionKeyName: {S: aws.String(key)},
			d.rangeKeyName:     {S: aws.String("value")},
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
	switch {
	case v.B != nil:
		raw = v.B
	case v.S != nil:
		raw = []byte(*v.S)
	case v.N != nil:
		raw = []byte(*v.N)
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

	_, err := d.writeClient.DeleteItemWithContext(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.table),
		Key: map[string]*dynamodb.AttributeValue{
			d.partitionKeyName: {
				S: aws.String(partitionKey),
			},
			d.rangeKeyName: {
				S: aws.String(rangeKey),
			},
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
	var exclusiveStartKey map[string]*dynamodb.AttributeValue
	if paginationToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(paginationToken)
		if err != nil {
			return nil, "", fmt.Errorf("invalid pagination token: %w", err)
		}
		err = json.Unmarshal(decoded, &exclusiveStartKey)
		if err != nil {
			return nil, "", fmt.Errorf("invalid pagination token format: %w", err)
		}
	}

	var result *dynamodb.ScanOutput
	var err error

	if index == ConnectorReverseIndex {
		// Use the reverse index (GSI)
		result, err = d.readClient.ScanWithContext(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(d.table),
			IndexName:         aws.String(d.reverseIndexName),
			Limit:             aws.Int64(int64(limit)),
			ExclusiveStartKey: exclusiveStartKey,
		})
	} else {
		// Use the main table
		result, err = d.readClient.ScanWithContext(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(d.table),
			Limit:             aws.Int64(int64(limit)),
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
		if ttl, exists := item[d.ttlAttributeName]; exists && ttl.N != nil && *ttl.N != "" && *ttl.N != "0" {
			expirationTime, err := strconv.ParseInt(*ttl.N, 10, 64)
			if err == nil && now > expirationTime {
				continue // Skip expired items
			}
		}

		// Extract partition and range keys
		partitionKey := ""
		rangeKey := ""
		if item[d.partitionKeyName] != nil && item[d.partitionKeyName].S != nil {
			partitionKey = *item[d.partitionKeyName].S
		}
		if item[d.rangeKeyName] != nil && item[d.rangeKeyName].S != nil {
			rangeKey = *item[d.rangeKeyName].S
		}

		// Extract value
		var value []byte
		if item["value"] != nil {
			if item["value"].B != nil {
				value = item["value"].B
			} else if item["value"].S != nil {
				value = []byte(*item["value"].S)
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
		tokenData, err := json.Marshal(result.LastEvaluatedKey)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create pagination token: %w", err)
		}
		nextToken = base64.StdEncoding.EncodeToString(tokenData)
	}

	return results, nextToken, nil
}
