package consensus

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// s3PutObjectAPI is the sliver of the S3 client this exporter uses. It is an
// interface so the buffering/flush logic can be tested without AWS.
type s3PutObjectAPI interface {
	PutObject(*s3.PutObjectInput) (*s3.PutObjectOutput, error)
}

// pendingBatch is the set of records that will become ONE S3 object.
//
// Records are grouped by (method, networkId) and the file name is resolved at
// FLUSH time. Grouping by the resolved name instead (the previous behavior) put
// every record in its own bucket, because the default pattern embeds
// {timestampMs}: counts never exceeded 1, so maxRecords/maxSize could never
// fire and the only surviving flush path was the periodic tick.
type pendingBatch struct {
	buf       bytes.Buffer
	count     int
	method    string
	networkId string
}

// s3MisbehaviorExporter implements buffered S3 uploads
type s3MisbehaviorExporter struct {
	mu        sync.Mutex
	cfg       *common.MisbehaviorsDestinationConfig
	log       *zerolog.Logger
	s3Client  s3PutObjectAPI
	bucket    string
	keyPrefix string

	// Buffered records awaiting upload, keyed by (method, networkId).
	batches map[string]*pendingBatch

	// Channel for async flush
	flushCh   chan struct{}
	closeCh   chan struct{}
	closeWg   sync.WaitGroup
	closeOnce sync.Once
}

// Close stops the flush worker and performs a final flush, so records buffered
// since the last interval survive a graceful shutdown. Without it every pod
// roll silently discarded up to FlushInterval worth of forensics — which is
// how a month of integrity catches went missing while the archive looked
// healthy (the only surviving batches were those whose ticker happened to fire
// before their pod was replaced). Safe to call more than once.
func (e *s3MisbehaviorExporter) Close() error {
	var err error
	e.closeOnce.Do(func() {
		close(e.closeCh)
		// Let the worker (if one is running) finish its own final flush first,
		// then flush again ourselves: flush() is a no-op on an empty batch set,
		// so this is harmless when the worker already drained, and it is the
		// ONLY flush when no worker was started.
		e.closeWg.Wait()
		e.mu.Lock()
		err = e.flush()
		e.mu.Unlock()
	})
	return err
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

	// S3-compatible providers (Tigris, MinIO, R2, …) — path-style keeps bucket
	// resolution off DNS, which every compatible provider supports.
	if cfg.S3.Endpoint != "" {
		awsConfig.Endpoint = aws.String(cfg.S3.Endpoint)
		awsConfig.S3ForcePathStyle = aws.Bool(true)
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
		cfg:       cfg,
		log:       log,
		s3Client:  s3Client,
		bucket:    bucket,
		keyPrefix: keyPrefix,
		batches:   make(map[string]*pendingBatch),
		flushCh:   make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
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
			// Periodic flush. The tick IS the interval, so flush whatever is
			// buffered. Re-checking elapsed time here made the flush skip every
			// other tick: the previous flush stamped its own completion time a
			// hair AFTER the tick that triggered it, so the next tick was always
			// a few microseconds short of flushInterval and did nothing —
			// doubling the worst-case time a record sat unwritten (and, with a
			// pod restart in between, losing it).
			e.mu.Lock()
			_ = e.flush()
			e.mu.Unlock()

		case <-e.flushCh:
			// Triggered flush
			e.mu.Lock()
			_ = e.flush()
			e.mu.Unlock()
		}
	}
}

// shouldFlush reports whether any batch has hit a size/record threshold (called
// with lock held). Time-based flushing belongs to the ticker in flushWorker.
func (e *s3MisbehaviorExporter) shouldFlush() bool {
	for _, b := range e.batches {
		if int64(b.buf.Len()) >= e.cfg.S3.MaxSize {
			return true
		}
		if b.count >= e.cfg.S3.MaxRecords {
			return true
		}
	}
	return false
}

// flush uploads the current buffer to S3 (called with lock held)
func (e *s3MisbehaviorExporter) flush() error {
	if len(e.batches) == 0 {
		return nil
	}
	now := time.Now()
	// Names already written in THIS pass. A pattern without {method}/{networkId}
	// resolves every batch to the same name, and an S3 PUT overwrites rather
	// than appends — so the second batch would silently erase the first.
	used := make(map[string]struct{}, len(e.batches))
	for groupKey, b := range e.batches {
		if b.buf.Len() == 0 {
			delete(e.batches, groupKey)
			continue
		}
		key := e.keyPrefix + uniqueName(resolveFilePatternWithDefaults(e.cfg, b.method, b.networkId, now), used)
		input := &s3.PutObjectInput{
			Bucket:      aws.String(e.bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(b.buf.Bytes()),
			ContentType: aws.String(e.cfg.S3.ContentType),
		}
		if _, err := e.s3Client.PutObject(input); err != nil {
			// Keep the batch buffered so the next flush retries it.
			e.log.Error().Err(err).Str("bucket", e.bucket).Str("key", key).Int("bytes", b.buf.Len()).Int("records", b.count).Msg("failed to upload misbehavior records to S3")
			continue
		}
		e.log.Info().Str("bucket", e.bucket).Str("key", key).Int("bytes", b.buf.Len()).Int("records", b.count).Msg("uploaded misbehavior records to S3")
		// Drop the batch entirely rather than Reset()ing it: a reset buffer
		// keeps its capacity, and these hold whole block bodies, so retaining
		// one per (method, networkId) ever seen leaked memory for the process's
		// lifetime.
		delete(e.batches, groupKey)
	}
	return nil
}

// uniqueName disambiguates name against the ones already used in this flush,
// inserting the suffix before the file extension (foo.jsonl -> foo-1.jsonl).
func uniqueName(name string, used map[string]struct{}) string {
	candidate := name
	for i := 1; ; i++ {
		if _, clash := used[candidate]; !clash {
			used[candidate] = struct{}{}
			return candidate
		}
		if dot := strings.LastIndex(name, "."); dot > 0 {
			candidate = fmt.Sprintf("%s-%d%s", name[:dot], i, name[dot:])
		} else {
			candidate = fmt.Sprintf("%s-%d", name, i)
		}
	}
}

// AppendWithMetadata adds a record to the appropriate buffer and flushes if necessary
func (e *s3MisbehaviorExporter) AppendWithMetadata(line []byte, method string, networkId string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Group by the metadata, not by the resolved file name — the name is
	// resolved at flush time so that records accumulate into one object.
	groupKey := method + "\x00" + networkId
	b := e.batches[groupKey]
	if b == nil {
		b = &pendingBatch{method: method, networkId: networkId}
		e.batches[groupKey] = b
	}

	// Add to buffer
	if _, err := b.buf.Write(line); err != nil {
		return err
	}
	if err := b.buf.WriteByte('\n'); err != nil {
		return err
	}
	b.count++

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
