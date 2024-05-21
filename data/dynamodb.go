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
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog/log"
)

const (
	DynamoDBStoreDriver = "dynamodb"
)

type DynamoDBStore struct {
	client *dynamodb.DynamoDB
	table  string
}

type DynamoDBValueWriter struct {
	ctx    context.Context
	client *dynamodb.DynamoDB
	table  string
	key    string
	buffer strings.Builder
}

func (w *DynamoDBValueWriter) Write(p []byte) (n int, err error) {
	// log.Trace().Msgf("writing to DynamoDBValueWriter key: %s", w.key)
	w.buffer.Write(p)
	return len(p), nil
}

func (w *DynamoDBValueWriter) Close() error {
	item := map[string]*dynamodb.AttributeValue{
		"key": {
			S: aws.String(w.key),
		},
		"value": {
			S: aws.String(w.buffer.String()),
		},
	}

	_, err := w.client.PutItemWithContext(w.ctx, &dynamodb.PutItemInput{
		TableName: aws.String(w.table),
		Item:      item,
	})

	return err
}

func NewDynamoDBStore(cfg *config.DynamoDBStoreConfig) (*DynamoDBStore, error) {
	log.Debug().Msgf("creating DynamoDBStore with config: %+v", cfg)

	sess, err := createSession(cfg)
	if err != nil {
		return nil, err
	}

	client := dynamodb.New(sess, &aws.Config{
		Endpoint: aws.String(cfg.Endpoint),
		HTTPClient: &http.Client{
			Timeout: 3 * time.Second,
		},
		LogLevel: aws.LogLevel(aws.LogDebugWithSigning | aws.LogDebugWithHTTPBody | aws.LogDebugWithRequestRetries | aws.LogDebugWithRequestErrors | aws.LogDebugWithEventStreamBody | aws.LogDebugWithDeprecated),
	})

	var tbl = "rpc_cache"
	if cfg.Table != "" {
		tbl = cfg.Table
	}

	client.CreateTable(&dynamodb.CreateTableInput{
		TableName: aws.String(tbl),
		KeySchema: []*dynamodb.KeySchemaElement{
			{
				AttributeName: aws.String("key"),
				KeyType:       aws.String("HASH"),
			},
		},
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			{
				AttributeName: aws.String("key"),
				AttributeType: aws.String("S"),
			},
		},
		BillingMode: aws.String("PAY_PER_REQUEST"),
	})

	return &DynamoDBStore{client: client, table: tbl}, nil
}

func createSession(cfg *config.DynamoDBStoreConfig) (*session.Session, error) {
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

func (d *DynamoDBStore) Get(ctx context.Context, key string) (string, error) {
	log.Trace().Msgf("DynamoDBStore getting key: %s", key)
	result, err := d.client.GetItemWithContext(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table),
		Key: map[string]*dynamodb.AttributeValue{
			"key": {
				S: aws.String(key),
			},
		},
	})
	if err != nil {
		return "", err
	}

	if result.Item == nil {
		return "", common.NewErrRecordNotFound(key, DynamoDBStoreDriver)
	}

	var item map[string]string
	err = dynamodbattribute.UnmarshalMap(result.Item, &item)
	if err != nil {
		return "", err
	}

	return item["value"], nil
}

func (d *DynamoDBStore) GetWithReader(ctx context.Context, key string) (io.Reader, error) {
	// log.Trace().Msgf("DynamoDBStore getting key with reader: %s", key)
	value, err := d.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return strings.NewReader(value), nil
}

func (d *DynamoDBStore) Set(ctx context.Context, key string, value string) (int, error) {
	// log.Trace().Msgf("DynamoDBStore setting key: %s, value: %s", key, value)
	item := map[string]*dynamodb.AttributeValue{
		"key": {
			S: aws.String(key),
		},
		"value": {
			S: aws.String(value),
		},
	}

	_, err := d.client.PutItemWithContext(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item:      item,
	})
	if err != nil {
		return 0, err
	}

	return len(value), nil
}

func (d *DynamoDBStore) SetWithWriter(ctx context.Context, key string) (io.WriteCloser, error) {
	// log.Trace().Msgf("DynamoDBStore setting key with writer: %s", key)
	return &DynamoDBValueWriter{client: d.client, table: d.table, key: key, ctx: ctx}, nil
}

func (d *DynamoDBStore) Scan(ctx context.Context, prefix string) ([]string, error) {
	var values []string

	err := d.client.ScanPagesWithContext(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(d.table),
		FilterExpression: aws.String("begins_with(#key, :prefix)"),
		ExpressionAttributeNames: map[string]*string{
			"#key": aws.String("key"),
		},
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":prefix": {
				S: aws.String(prefix),
			},
		},
	}, func(page *dynamodb.ScanOutput, lastPage bool) bool {
		for _, item := range page.Items {
			var result map[string]string
			if err := dynamodbattribute.UnmarshalMap(item, &result); err == nil {
				values = append(values, result["key"])
			}
		}
		return !lastPage
	})

	if err != nil {
		return nil, err
	}

	return values, nil
}

func (d *DynamoDBStore) Delete(ctx context.Context, key string) error {
	_, err := d.client.DeleteItemWithContext(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.table),
		Key: map[string]*dynamodb.AttributeValue{
			"key": {
				S: aws.String(key),
			},
		},
	})

	return err
}
