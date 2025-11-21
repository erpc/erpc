package consensus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

var (
	instanceID     string
	instanceIDOnce sync.Once
)

// getInstanceID returns a unique identifier for this instance.
// Priority order:
// 1. INSTANCE_ID environment variable
// 2. POD_NAME environment variable (Kubernetes)
// 3. HOSTNAME environment variable
// 4. Generated hash based on startup time and process ID
func getInstanceID() string {
	instanceIDOnce.Do(func() {
		// Try environment variables first
		if id := os.Getenv("INSTANCE_ID"); id != "" {
			instanceID = sanitizeForFilename(id)
			return
		}
		if id := os.Getenv("POD_NAME"); id != "" {
			instanceID = sanitizeForFilename(id)
			return
		}
		if id := os.Getenv("HOSTNAME"); id != "" {
			instanceID = sanitizeForFilename(id)
			return
		}

		// Generate a deterministic ID based on startup time and process
		h := sha256.New()
		h.Write([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())))
		hash := hex.EncodeToString(h.Sum(nil))
		instanceID = hash[:8] // Use first 8 chars of hash
	})
	return instanceID
}

// sanitizeForFilename replaces characters that are problematic in filenames
func sanitizeForFilename(s string) string {
	replacer := strings.NewReplacer(
		":", "_",
		"/", "_",
		"\\", "_",
		" ", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		"\n", "_",
		"\r", "_",
		"\t", "_",
	)
	return replacer.Replace(s)
}

// resolveFilePattern resolves placeholders in the file pattern.
// Supported placeholders:
// - {dateByHour}: formatted as 2006-01-02-15
// - {dateByDay}: formatted as 2006-01-02
// - {method}: the RPC method name
// - {networkId}: the network ID with : replaced by _
// - {instanceId}: unique instance identifier
func resolveFilePattern(pattern string, method string, networkId string, timestamp time.Time) string {
	// No defaulting here; defaults are applied during config SetDefaults

	replacements := map[string]string{
		"{dateByHour}":  timestamp.UTC().Format("2006-01-02-15"),
		"{dateByDay}":   timestamp.UTC().Format("2006-01-02"),
		"{method}":      sanitizeForFilename(method),
		"{networkId}":   sanitizeForFilename(networkId),
		"{instanceId}":  getInstanceID(),
		"{timestampMs}": fmt.Sprintf("%d", timestamp.UTC().UnixMilli()),
	}

	result := pattern
	for placeholder, value := range replacements {
		result = strings.ReplaceAll(result, placeholder, value)
	}

	if !strings.HasSuffix(result, ".ndjson") && !strings.HasSuffix(result, ".jsonl") {
		result += ".jsonl"
	}

	return result
}

// resolveFilePatternWithDefaults uses different defaults based on destination type
func resolveFilePatternWithDefaults(cfg *common.MisbehaviorsDestinationConfig, method string, networkId string, timestamp time.Time) string {
	pattern := cfg.FilePattern
	return resolveFilePattern(pattern, method, networkId, timestamp)
}

// parseS3Path parses an S3 URI into bucket and key prefix
// Format: s3://bucket-name/optional/prefix/path/
func parseS3Path(s3Path string) (bucket string, keyPrefix string, err error) {
	if !strings.HasPrefix(s3Path, "s3://") {
		return "", "", fmt.Errorf("invalid S3 path: must start with s3://")
	}

	path := strings.TrimPrefix(s3Path, "s3://")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) == 0 || parts[0] == "" {
		return "", "", fmt.Errorf("invalid S3 path: no bucket specified")
	}

	bucket = parts[0]
	if len(parts) > 1 {
		keyPrefix = parts[1]
		// Ensure prefix ends with / if not empty
		if keyPrefix != "" && !strings.HasSuffix(keyPrefix, "/") {
			keyPrefix += "/"
		}
	}

	return bucket, keyPrefix, nil
}
