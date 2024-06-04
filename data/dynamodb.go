package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog/log"
)

const (
	DynamoDBDriverName = "dynamodb"
)

var _ Connector = (*DynamoDBConnector)(nil)

type DynamoDBConnector struct {
	client           *dynamodb.DynamoDB
	table            string
	partitionKeyName string
	rangeKeyName     string
	reverseIndexName string
	keyResolver      KeyResolver
	valueResolver    ValueResolver
}

type DynamoDBValueWriter struct {
	ctx           context.Context
	connector     *DynamoDBConnector
	partitionKey  string
	rangeKey      string
	buffer        *strings.Builder
	keyResolver   KeyResolver
	valueResolver ValueResolver
}

func (w *DynamoDBValueWriter) Write(p []byte) (n int, err error) {
	w.buffer.Write(p)
	return len(p), nil
}

func (w *DynamoDBValueWriter) Close() error {
	var pk string
	var rk string
	var err error

	dv := &DataValue{
		raw: w.buffer.String(),
	}

	if w.partitionKey != "" && w.rangeKey != "" {
		pk = w.partitionKey
		rk = w.rangeKey
	} else if w.keyResolver != nil {
		pk, rk, err = w.keyResolver(w.ctx, dv)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("missing partition and range keys for dynamodb, also no keyResolver provided")
	}

	
	if w.valueResolver != nil {
		dv, err = w.valueResolver(w.ctx, dv)
		if err != nil {
			return err
		}
	}
	
	log.Debug().Interface("data", dv).Msgf("writing item to dynamodb with partition key: %s and range key: %s", pk, rk)

	if dv == nil {
		// Skip writing since valueResolver returned nil
		return nil
	}

	if pk == "" || rk == "" {
		// Skip when key resolver returns empty keys (i.e. cache must be ignored)
		return nil
	}

	item := map[string]*dynamodb.AttributeValue{
		w.connector.partitionKeyName: {
			S: aws.String(pk),
		},
		w.connector.rangeKeyName: {
			S: aws.String(rk),
		},
		"value": {
			S: aws.String(dv.raw),
		},
	}

	_, err = w.connector.client.PutItemWithContext(w.ctx, &dynamodb.PutItemInput{
		TableName: aws.String(w.connector.table),
		Item:      item,
	})

	return err
}

func NewDynamoDBConnector(
	ctx context.Context,
	cfg *config.DynamoDBConnectorConfig,
	keyResolver KeyResolver,
	valueResolver ValueResolver,
) (*DynamoDBConnector, error) {
	log.Debug().Msgf("creating DynamoDBConnector with config: %+v", cfg)

	sess, err := createSession(cfg)
	if err != nil {
		return nil, err
	}

	client := dynamodb.New(sess, &aws.Config{
		Endpoint: aws.String(cfg.Endpoint),
		HTTPClient: &http.Client{
			Timeout: 3 * time.Second,
		},
	})

	if cfg.Table == "" {
		return nil, fmt.Errorf("missing table name for dynamodb connector")
	}

	sctx, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()

	err = createTableIfNotExists(sctx, client, cfg)
	if err != nil {
		return nil, err
	}

	err = ensureGlobalSecondaryIndexes(sctx, client, cfg)
	if err != nil {
		return nil, err
	}

	return &DynamoDBConnector{
		client:           client,
		table:            cfg.Table,
		partitionKeyName: cfg.PartitionKeyName,
		rangeKeyName:     cfg.RangeKeyName,
		reverseIndexName: cfg.ReverseIndexName,
		keyResolver:      keyResolver,
		valueResolver:    valueResolver,
	}, nil
}

func createSession(cfg *config.DynamoDBConnectorConfig) (*session.Session, error) {
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
	client *dynamodb.DynamoDB,
	cfg *config.DynamoDBConnectorConfig,
) error {
	log.Debug().Msgf("creating dynamodb table '%s' if not exists with partition key '%s' and range key %s", cfg.Table, cfg.PartitionKeyName, cfg.RangeKeyName)
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
		log.Error().Err(err).Msgf("failed to create dynamodb table %s", cfg.Table)
		return err
	}

	log.Debug().Msgf("dynamodb table '%s' is ready", cfg.Table)

	return nil
}

func ensureGlobalSecondaryIndexes(
	ctx context.Context,
	client *dynamodb.DynamoDB,
	cfg *config.DynamoDBConnectorConfig,
) error {
	if cfg.ReverseIndexName == "" {
		return nil
	}

	log.Debug().Msgf("ensuring global secondary index '%s' for table '%s'", cfg.ReverseIndexName, cfg.Table)

	currentTable, err := client.DescribeTableWithContext(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(cfg.Table),
	})

	if err != nil {
		return err
	}

	for _, gsi := range currentTable.Table.GlobalSecondaryIndexes {
		if *gsi.IndexName == cfg.ReverseIndexName {
			log.Debug().Msgf("global secondary index '%s' already exists", cfg.ReverseIndexName)
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
		log.Error().Err(err).Msgf("failed to create global secondary index '%s' for table %s", cfg.ReverseIndexName, cfg.Table)
	} else {
		log.Debug().Msgf("global secondary index '%s' created for table %s", cfg.ReverseIndexName, cfg.Table)
	}

	return err
}

func (d *DynamoDBConnector) SetWithWriter(ctx context.Context, partitionKey, rangeKey string) (io.WriteCloser, error) {
	log.Debug().Msgf("creating DynamoDBValueWriter with partition key: %s and range key: %s", partitionKey, rangeKey)
	return &DynamoDBValueWriter{
		connector:     d,
		partitionKey:  partitionKey,
		rangeKey:      rangeKey,
		ctx:           ctx,
		buffer:        &strings.Builder{},
		keyResolver:   d.keyResolver,
		valueResolver: d.valueResolver,
	}, nil
}

func (d *DynamoDBConnector) GetWithReader(ctx context.Context, index, partitionKey, rangeKey string) (io.Reader, error) {
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
		log.Debug().Msgf("querying dynamodb with input: %+v", qi)
		result, err := d.client.QueryWithContext(ctx, qi)
		if err != nil {
			return nil, err
		}
		if len(result.Items) == 0 {
			return nil, common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), DynamoDBDriverName)
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
		log.Debug().Msgf("getting item from dynamodb with key: %+v", ky)
		result, err := d.client.GetItemWithContext(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(d.table),
			Key:       ky,
		})

		if err != nil {
			return nil, err
		}

		if result.Item == nil {
			return nil, common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), DynamoDBDriverName)
		}

		value = *result.Item["value"].S
	}

	return strings.NewReader(value), nil
}

func (d *DynamoDBConnector) Query(ctx context.Context, index, partitionKey, rangeKey string) ([]*DataValue, error) {
	var keyCondition string
	var exprAttrNames = map[string]*string{
		"#pkey": aws.String(d.partitionKeyName),
	}
	var exprAttrValues = map[string]*dynamodb.AttributeValue{}

	if strings.HasSuffix(partitionKey, "*") && index == ConnectorMainIndex {
		return nil, fmt.Errorf("partition key does not support prefix search in dynamodb")
	} else {
		keyCondition = "#pkey = :pkey"
		exprAttrValues[":pkey"] = &dynamodb.AttributeValue{
			S: aws.String(partitionKey),
		}
	}

	if rangeKey != "" {
		exprAttrNames["#rkey"] = aws.String(d.rangeKeyName)
		exprAttrValues[":rkey"] = &dynamodb.AttributeValue{
			S: aws.String(strings.TrimSuffix(rangeKey, "*")),
		}

		if strings.HasSuffix(rangeKey, "*") {
			keyCondition += " AND begins_with(#rkey, :rkey)"
		} else {
			keyCondition += " AND #rkey = :rkey"
		}
	}

	qi := &dynamodb.QueryInput{
		TableName:                 aws.String(d.table),
		KeyConditionExpression:    aws.String(keyCondition),
		ExpressionAttributeNames:  exprAttrNames,
		ExpressionAttributeValues: exprAttrValues,
	}

	if index != "" && index != "main" {
		qi.IndexName = aws.String(index)
	}

	var results []*DataValue
	var lastEvaluatedKey map[string]*dynamodb.AttributeValue

	for {
		if lastEvaluatedKey != nil {
			qi.ExclusiveStartKey = lastEvaluatedKey
		}

		log.Debug().Msgf("querying dynamodb with input: %+v", qi)
		result, err := d.client.QueryWithContext(ctx, qi)
		if err != nil {
			return nil, err
		}

		for _, item := range result.Items {
			dv := &DataValue{
				raw: *item["value"].S,
			}
			results = append(results, dv)
		}

		lastEvaluatedKey = result.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return results, nil
}

func (d *DynamoDBConnector) Delete(ctx context.Context, index, partitionKey, rangeKey string) error {
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
