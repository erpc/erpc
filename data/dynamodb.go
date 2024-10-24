package data

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

const (
	DynamoDBDriverName = "dynamodb"
)

var _ Connector = (*DynamoDBConnector)(nil)

type DynamoDBConnector struct {
	logger           *zerolog.Logger
	client           *dynamodb.DynamoDB
	table            string
	partitionKeyName string
	rangeKeyName     string
	reverseIndexName string
}

func NewDynamoDBConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.DynamoDBConnectorConfig,
) (*DynamoDBConnector, error) {
	logger.Debug().Msgf("creating DynamoDBConnector with config: %+v", cfg)

	connector := &DynamoDBConnector{
		logger:           logger,
		table:            cfg.Table,
		partitionKeyName: cfg.PartitionKeyName,
		rangeKeyName:     cfg.RangeKeyName,
		reverseIndexName: cfg.ReverseIndexName,
	}

	// Attempt the actual connecting in background to avoid blocking the main thread.
	go func() {
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				logger.Error().Msg("Context cancelled while attempting to connect to DynamoDB")
				return
			default:
				logger.Debug().Msgf("attempting to connect to DynamoDB (attempt %d of 30)", i+1)
				err := connector.connect(ctx, cfg)
				if err == nil {
					return
				}
				logger.Warn().Msgf("failed to connect to DynamoDB (attempt %d of 30): %s", i+1, err)
				time.Sleep(10 * time.Second)
			}
		}
		logger.Error().Msg("Failed to connect to DynamoDB after maximum attempts")
	}()

	return connector, nil
}

func (d *DynamoDBConnector) connect(ctx context.Context, cfg *common.DynamoDBConnectorConfig) error {
	sess, err := createSession(cfg)
	if err != nil {
		return err
	}

	d.client = dynamodb.New(sess, &aws.Config{
		Endpoint: aws.String(cfg.Endpoint),
		HTTPClient: &http.Client{
			Timeout: 3 * time.Second,
		},
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
				Timeout: 3 * time.Second,
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
			Timeout: 3 * time.Second,
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

func (d *DynamoDBConnector) SetTTL(_ string, _ string) error {
	d.logger.Debug().Msgf("Method TTLs not implemented for DynamoDBConnector")
	return nil
}

func (d *DynamoDBConnector) HasTTL(_ string) bool {
	return false
}

func (d *DynamoDBConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	if d.client == nil {
		return fmt.Errorf("DynamoDB client not initialized yet")
	}

	d.logger.Debug().Msgf("writing to dynamodb with partition key: %s and range key: %s", partitionKey, rangeKey)

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

	_, err := d.client.PutItemWithContext(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item:      item,
	})

	return err
}

func (d *DynamoDBConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if d.client == nil {
		return "", fmt.Errorf("DynamoDB client not initialized yet")
	}

	var value string

	if index == ConnectorReverseIndex {
		var keyCondition string = ""
		var exprAttrNames map[string]*string = map[string]*string{
			"#pkey": aws.String(d.partitionKeyName),
		}
		var exprAttrValues map[string]*dynamodb.AttributeValue = map[string]*dynamodb.AttributeValue{
			":pkey": {
				S: aws.String(strings.TrimSuffix(partitionKey, "*")),
			},
		}
		if strings.HasSuffix(partitionKey, "*") {
			keyCondition = "begins_with(#pkey, :pkey)"
		} else {
			keyCondition = "#pkey = :pkey"
		}
		if rangeKey != "" {
			if strings.HasSuffix(rangeKey, "*") {
				keyCondition += " AND begins_with(#rkey, :rkey)"
			} else {
				keyCondition += " AND #rkey = :rkey"
			}
			exprAttrNames["#rkey"] = aws.String(d.rangeKeyName)
			exprAttrValues[":rkey"] = &dynamodb.AttributeValue{
				S: aws.String(strings.TrimSuffix(rangeKey, "*")),
			}
		}
		qi := &dynamodb.QueryInput{
			TableName:                 aws.String(d.table),
			IndexName:                 aws.String(d.reverseIndexName),
			KeyConditionExpression:    aws.String(keyCondition),
			ExpressionAttributeNames:  exprAttrNames,
			ExpressionAttributeValues: exprAttrValues,
		}
		d.logger.Debug().Msgf("getting item from dynamodb with input: %+v", qi)
		result, err := d.client.QueryWithContext(ctx, qi)
		if err != nil {
			return "", err
		}
		if len(result.Items) == 0 {
			return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), DynamoDBDriverName)
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
		d.logger.Debug().Msgf("getting item from dynamodb with key: %+v", ky)
		result, err := d.client.GetItemWithContext(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(d.table),
			Key:       ky,
		})

		if err != nil {
			return "", err
		}

		if result.Item == nil {
			return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), DynamoDBDriverName)
		}

		value = *result.Item["value"].S
	}

	return value, nil
}

func (d *DynamoDBConnector) Delete(ctx context.Context, index, partitionKey, rangeKey string) error {
	if d.client == nil {
		return fmt.Errorf("DynamoDB client not initialized yet")
	}

	if strings.HasSuffix(rangeKey, "*") {
		return d.deleteWithPrefix(ctx, index, partitionKey, rangeKey)
	} else {
		return d.deleteSingleItem(ctx, partitionKey, rangeKey)
	}
}

func (d *DynamoDBConnector) deleteWithPrefix(ctx context.Context, index, partitionKey, rangeKey string) error {
	var keyCondition string = "#pkey = :pkey AND begins_with(#rkey, :rkey)"
	var exprAttrNames = map[string]*string{
		"#pkey": aws.String(d.partitionKeyName),
		"#rkey": aws.String(d.rangeKeyName),
	}
	var exprAttrValues = map[string]*dynamodb.AttributeValue{
		":pkey": {
			S: aws.String(partitionKey),
		},
		":rkey": {
			S: aws.String(strings.TrimSuffix(rangeKey, "*")),
		},
	}

	qi := &dynamodb.QueryInput{
		TableName:                 aws.String(d.table),
		KeyConditionExpression:    aws.String(keyCondition),
		ExpressionAttributeNames:  exprAttrNames,
		ExpressionAttributeValues: exprAttrValues,
	}

	if index != "" {
		qi.IndexName = aws.String(index)
	}

	var lastEvaluatedKey map[string]*dynamodb.AttributeValue
	for {
		if lastEvaluatedKey != nil {
			qi.ExclusiveStartKey = lastEvaluatedKey
		}

		result, err := d.client.QueryWithContext(ctx, qi)
		if err != nil {
			return err
		}

		if len(result.Items) == 0 {
			break
		}

		var keys []map[string]*dynamodb.AttributeValue
		for _, item := range result.Items {
			keys = append(keys, map[string]*dynamodb.AttributeValue{
				d.partitionKeyName: item[d.partitionKeyName],
				d.rangeKeyName:     item[d.rangeKeyName],
			})

			if len(keys) == 25 {
				err = d.deleteKeys(ctx, keys)
				if err != nil {
					return err
				}
				keys = keys[:0] // reset the keys slice
			}
		}

		if len(keys) > 0 {
			err = d.deleteKeys(ctx, keys)
			if err != nil {
				return err
			}
		}

		lastEvaluatedKey = result.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return nil
}

func (d *DynamoDBConnector) deleteSingleItem(ctx context.Context, partitionKey, rangeKey string) error {
	_, err := d.client.DeleteItemWithContext(ctx, &dynamodb.DeleteItemInput{
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

	return err
}

func (d *DynamoDBConnector) deleteKeys(ctx context.Context, keys []map[string]*dynamodb.AttributeValue) error {
	var deleteRequests []*dynamodb.WriteRequest

	for _, key := range keys {
		deleteRequests = append(deleteRequests, &dynamodb.WriteRequest{
			DeleteRequest: &dynamodb.DeleteRequest{
				Key: key,
			},
		})
	}
	_, err := d.client.BatchWriteItemWithContext(ctx, &dynamodb.BatchWriteItemInput{
		RequestItems: map[string][]*dynamodb.WriteRequest{
			d.table: deleteRequests,
		},
	})

	return err
}
