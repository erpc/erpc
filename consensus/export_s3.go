package consensus

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// s3MisbehaviorExporter implements buffered S3 uploads
type s3MisbehaviorExporter struct {
	mu        sync.Mutex
	cfg       *common.MisbehaviorsDestinationConfig
	log       *zerolog.Logger
	s3Client  *s3.S3
	bucket    string
	keyPrefix string

	// Per-key buffers and counters (key := resolved file name)
	buffers           map[string]*bytes.Buffer
	counts            map[string]int
	lastPeriodicFlush time.Time

	// Channel for async flush
	flushCh chan struct{}
	closeCh chan struct{}
	closeWg sync.WaitGroup
}

func newS3MisbehaviorExporter(cfg *common.MisbehaviorsDestinationConfig, log *zerolog.Logger) (*s3MisbehaviorExporter, error) {
	if cfg == nil || cfg.Path == "" {
		return nil, fmt.Errorf("empty S3 path configuration")
	}

	// Parse S3 path
	bucket, keyPrefix, err := parseS3Path(cfg.Path)
	if err != nil {
		return nil, err
	}

	// Create AWS session with tuned HTTP transport for better connection reuse
	awsConfig := &aws.Config{
		Region: aws.String(cfg.S3.Region),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 60 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          256,
				MaxIdleConnsPerHost:   256,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			Timeout: 0,
		},
		MaxRetries: aws.Int(5),
	}

	// Configure credentials if provided
	if cfg.S3.Credentials != nil {
		switch cfg.S3.Credentials.Mode {
		case "secret":
			awsConfig.Credentials = credentials.NewStaticCredentials(
				cfg.S3.Credentials.AccessKeyID,
				cfg.S3.Credentials.SecretAccessKey,
				"",
			)
		case "file":
			awsConfig.Credentials = credentials.NewSharedCredentials(
				cfg.S3.Credentials.CredentialsFile,
				cfg.S3.Credentials.Profile,
			)
		case "env":
			// Use environment variables (default behavior)
			awsConfig.Credentials = credentials.NewEnvCredentials()
		default:
			// Use default credential chain (env, IAM role, etc.)
		}
	}
	// If no credentials specified, AWS SDK will use default chain:
	// 1. Environment variables
	// 2. Shared credentials file
	// 3. IAM role (for EC2/ECS/EKS)

	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %w", err)
	}

	s3Client := s3.New(sess)

	// Verify bucket access
	_, err = s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return nil, fmt.Errorf("unable to access S3 bucket %s: %w", bucket, err)
	}

	exp := &s3MisbehaviorExporter{
		cfg:               cfg,
		log:               log,
		s3Client:          s3Client,
		bucket:            bucket,
		keyPrefix:         keyPrefix,
		buffers:           make(map[string]*bytes.Buffer),
		counts:            make(map[string]int),
		lastPeriodicFlush: time.Now(),
		flushCh:           make(chan struct{}, 1),
		closeCh:           make(chan struct{}),
	}

	// Start background flush worker
	exp.closeWg.Add(1)
	go exp.flushWorker()

	return exp, nil
}

// flushWorker handles periodic flushing in the background
func (e *s3MisbehaviorExporter) flushWorker() {
	defer e.closeWg.Done()

	ticker := time.NewTicker(e.cfg.S3.FlushInterval.Duration())
	defer ticker.Stop()

	for {
		select {
		case <-e.closeCh:
			// Final flush before closing
			e.mu.Lock()
			_ = e.flush()
			e.mu.Unlock()
			return

		case <-ticker.C:
			// Periodic flush
			e.mu.Lock()
			if e.shouldFlush() {
				_ = e.flush()
			}
			e.mu.Unlock()

		case <-e.flushCh:
			// Triggered flush
			e.mu.Lock()
			_ = e.flush()
			e.mu.Unlock()
		}
	}
}

// shouldFlush determines if buffer should be flushed (called with lock held)
func (e *s3MisbehaviorExporter) shouldFlush() bool {
	// Time-based periodic flush when anything exists
	if len(e.buffers) == 0 {
		return false
	}
	if time.Since(e.lastPeriodicFlush) >= e.cfg.S3.FlushInterval.Duration() {
		return true
	}
	// Size or record thresholds on any buffer
	for key := range e.buffers {
		if int64(e.buffers[key].Len()) >= e.cfg.S3.MaxSize {
			return true
		}
		if e.counts[key] >= e.cfg.S3.MaxRecords {
			return true
		}
	}
	return false
}

// flush uploads the current buffer to S3 (called with lock held)
func (e *s3MisbehaviorExporter) flush() error {
	if len(e.buffers) == 0 {
		return nil
	}
	now := time.Now()
	for fileName, buf := range e.buffers {
		if buf == nil || buf.Len() == 0 {
			continue
		}
		key := e.keyPrefix + fileName
		input := &s3.PutObjectInput{
			Bucket:      aws.String(e.bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(buf.Bytes()),
			ContentType: aws.String(e.cfg.S3.ContentType),
		}
		if _, err := e.s3Client.PutObject(input); err != nil {
			e.log.Error().Err(err).Str("bucket", e.bucket).Str("key", key).Int("bytes", buf.Len()).Int("records", e.counts[fileName]).Msg("failed to upload misbehavior records to S3")
			continue
		}
		e.log.Info().Str("bucket", e.bucket).Str("key", key).Int("bytes", buf.Len()).Int("records", e.counts[fileName]).Msg("uploaded misbehavior records to S3")
		buf.Reset()
		e.counts[fileName] = 0
	}
	e.lastPeriodicFlush = now
	return nil
}

// AppendWithMetadata adds a record to the appropriate buffer and flushes if necessary
func (e *s3MisbehaviorExporter) AppendWithMetadata(line []byte, method string, networkId string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Derive fileName directly from provided metadata to respect user pattern
	fileName := resolveFilePatternWithDefaults(e.cfg, method, networkId, time.Now())

	// Get/Create buffer for this key
	buf := e.buffers[fileName]
	if buf == nil {
		buf = new(bytes.Buffer)
		e.buffers[fileName] = buf
	}

	// Add to buffer
	if _, err := buf.Write(line); err != nil {
		return err
	}
	if _, err := buf.Write([]byte("\n")); err != nil {
		return err
	}
	e.counts[fileName]++

	// Check if we should flush
	if e.shouldFlush() {
		// Trigger async flush
		select {
		case e.flushCh <- struct{}{}:
		default:
			// Channel is full, flush already pending
		}
	}

	return nil
}
