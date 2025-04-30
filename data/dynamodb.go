package data

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	DynamoDBDriverName = "dynamodb"
)

var _ Connector = (*DynamoDBConnector)(nil)

type DynamoDBConnector struct {
	id                string
	logger            *zerolog.Logger
	initializer       *util.Initializer
	client            *dynamodb.DynamoDB
	table             string
	ttlAttributeName  string
	partitionKeyName  string
	rangeKeyName      string
	reverseIndexName  string
	initTimeout       time.Duration
	getTimeout        time.Duration
	setTimeout        time.Duration
	statePollInterval time.Duration
}

type dynamoLock struct {
	connector *DynamoDBConnector
	lockKey   string
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
		return err
	}

	d.client = dynamodb.New(sess, &aws.Config{
		Endpoint: aws.String(cfg.Endpoint),
		HTTPClient: &http.Client{
			Timeout: d.setTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				MaxConnsPerHost:     100,
				IdleConnTimeout:     120 * time.Second,
			},
		},
		MaxRetries: aws.Int(3),
		Region:     aws.String(cfg.Region),
	})

	if cfg.Table == "" {
		return fmt.Errorf("missing table name for dynamodb connector")
	}

	err = createTableIfNotExists(ctx, d.logger, d.client, cfg)
	if err != nil {
		return err
	}

	err = ensureGlobalSecondaryIndexes(ctx, d.logger, d.client, cfg)
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

func (d *DynamoDBConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Set")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
			attribute.Int("value_size", len(value)),
		)
	}

	if d.client == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	if len(value) > 1024 {
		d.logger.Debug().Int("len", len(value)).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Interface("ttl", ttl).Msg("putting item in dynamodb")
	} else {
		d.logger.Debug().Str("value", value).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Interface("ttl", ttl).Msg("putting item in dynamodb")
	}

	item := map[string]*dynamodb.AttributeValue{
		d.partitionKeyName: {
			S: aws.String(partitionKey),
		},
		d.rangeKeyName: {
			S: aws.String(rangeKey),
		},
		"value": {
			S: aws.String(value),
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

	_, err := d.client.PutItemWithContext(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item:      item,
	})

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (d *DynamoDBConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Get")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	if d.client == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return "", err
	}

	var value string

	if index == ConnectorReverseIndex {
		if rangeKey == "" || strings.HasSuffix(rangeKey, "*") {
			err := fmt.Errorf("when using reverse index rangeKey must be a non-empty string and not contain wildcards (rangeKey: '%s', partitionKey: '%s')", rangeKey, partitionKey)
			common.SetTraceSpanError(span, err)
			return "", err
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

		qi := &dynamodb.QueryInput{
			TableName:                 aws.String(d.table),
			IndexName:                 aws.String(d.reverseIndexName),
			KeyConditionExpression:    aws.String(keyCondition),
			ExpressionAttributeNames:  exprAttrNames,
			ExpressionAttributeValues: exprAttrValues,
		}

		ctx, cancel := context.WithTimeout(ctx, d.getTimeout)
		defer cancel()

		d.logger.Debug().Str("index", d.reverseIndexName).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("getting item from dynamodb")
		result, err := d.client.QueryWithContext(ctx, qi)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return "", err
		}
		if len(result.Items) == 0 {
			err := common.NewErrRecordNotFound(partitionKey, rangeKey, DynamoDBDriverName)
			common.SetTraceSpanError(span, err)
			return "", err
		}

		// Check if the item has expired
		if ttl, exists := result.Items[0][d.ttlAttributeName]; exists && ttl.N != nil && *ttl.N != "" && *ttl.N != "0" {
			expirationTime, err := strconv.ParseInt(*ttl.N, 10, 64)
			now := time.Now().Unix()
			if err == nil && now > expirationTime {
				err := common.NewErrRecordExpired(partitionKey, rangeKey, DynamoDBDriverName, now, expirationTime)
				common.SetTraceSpanError(span, err)
				return "", err
			}
		}

		value = *result.Items[0]["value"].S
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
		result, err := d.client.GetItemWithContext(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(d.table),
			Key:       ky,
		})

		if err != nil {
			common.SetTraceSpanError(span, err)
			return "", err
		}

		if result.Item == nil {
			err := common.NewErrRecordNotFound(partitionKey, rangeKey, DynamoDBDriverName)
			common.SetTraceSpanError(span, err)
			return "", err
		}

		// Check if the item has expired
		if ttl, exists := result.Item[d.ttlAttributeName]; exists && ttl.N != nil && *ttl.N != "" && *ttl.N != "0" {
			expirationTime, err := strconv.ParseInt(*ttl.N, 10, 64)
			now := time.Now().Unix()
			if err == nil && now > expirationTime {
				err := common.NewErrRecordExpired(partitionKey, rangeKey, DynamoDBDriverName, now, expirationTime)
				common.SetTraceSpanError(span, err)
				return "", err
			}
		}

		value = *result.Item["value"].S
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

	if d.client == nil {
		err := fmt.Errorf("DynamoDB client not initialized yet")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	lockKey := fmt.Sprintf("%s:lock", key)
	expiryTime := time.Now().Add(ttl).Unix()

	_, err := d.client.PutItemWithContext(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item: map[string]*dynamodb.AttributeValue{
			d.partitionKeyName: {S: aws.String(lockKey)},
			d.rangeKeyName:     {S: aws.String("lock")},
			"expiry":           {N: aws.String(fmt.Sprintf("%d", expiryTime))},
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

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == dynamodb.ErrCodeConditionalCheckFailedException {
			err := common.NewErrLockAlreadyHeld(aerr)
			common.SetTraceSpanError(span, err)
			return nil, err
		}
		err := fmt.Errorf("failed to acquire lock: %w", err)
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	d.logger.Debug().Str("lockKey", lockKey).Dur("ttl", ttl).Msg("distributed lock acquired")

	return &dynamoLock{
		connector: d,
		lockKey:   lockKey,
	}, nil
}

func (l *dynamoLock) Unlock(ctx context.Context) error {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.Unlock",
		trace.WithAttributes(
			attribute.String("lock_key", l.lockKey),
		),
	)
	defer span.End()

	_, err := l.connector.client.DeleteItemWithContext(ctx, &dynamodb.DeleteItemInput{
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

func (d *DynamoDBConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	if d.client == nil {
		return nil, nil, fmt.Errorf("DynamoDB client not initialized yet")
	}

	updates := make(chan int64, 1)

	// Start polling goroutine
	ticker := time.NewTicker(d.statePollInterval)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		var lastValue int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				value, err := d.getSimpleValue(ctx, key)
				if err != nil {
					d.logger.Warn().Err(err).Str("key", key).Msg("failed to poll counter value")
					continue
				}
				if value > lastValue {
					lastValue = value
					select {
					case updates <- value:
					default:
					}
				}
			}
		}
	}()

	// Get initial value
	if val, err := d.getSimpleValue(ctx, key); err == nil {
		updates <- val
	}

	cleanup := func() {
		close(done)
		close(updates)
	}

	return updates, cleanup, nil
}

func (d *DynamoDBConnector) getSimpleValue(ctx context.Context, key string) (int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "DynamoDBConnector.getSimpleValue",
		trace.WithAttributes(
			attribute.String("key", key),
		),
	)
	defer span.End()

	result, err := d.client.GetItemWithContext(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table),
		Key: map[string]*dynamodb.AttributeValue{
			d.partitionKeyName: {S: aws.String(key)},
			d.rangeKeyName:     {S: aws.String("value")},
		},
		ConsistentRead: aws.Bool(true),
	})

	if err != nil {
		return 0, err
	}

	if result.Item == nil {
		return 0, nil
	}

	var value int64

	if v, ok := result.Item["value"]; ok && v.S != nil {
		value, _ = strconv.ParseInt(*v.S, 0, 64)
	} else if v, ok := result.Item["value"]; ok && v.N != nil {
		value, _ = strconv.ParseInt(*v.N, 0, 64)
	} else {
		return 0, fmt.Errorf("invalid value type for counter: %T", result.Item["value"])
	}

	return value, nil
}

func (d *DynamoDBConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	ctx, span := common.StartSpan(ctx, "DynamoDBConnector.PublishCounterInt64",
		trace.WithAttributes(
			attribute.String("key", key),
		),
	)
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("value", value),
		)
	}

	if d.client == nil {
		return fmt.Errorf("DynamoDB client not initialized yet")
	}

	_, err := d.client.UpdateItemWithContext(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key: map[string]*dynamodb.AttributeValue{
			d.partitionKeyName: {S: aws.String(key)},
			d.rangeKeyName:     {S: aws.String("value")},
		},
		UpdateExpression: aws.String(
			"SET #value = :value",
		),
		ConditionExpression: aws.String(
			"attribute_not_exists(#value) OR #value < :value",
		),
		ExpressionAttributeNames: map[string]*string{
			"#value": aws.String("value"),
		},
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":value": {N: aws.String(fmt.Sprintf("%d", value))},
		},
	})

	// Ignore condition check failures as they just mean the value wasn't higher
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == dynamodb.ErrCodeConditionalCheckFailedException {
			return nil
		}
		return err
	}

	return nil
}
