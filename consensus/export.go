package consensus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// misbehaviorExporter appends encoded JSONL lines to a storage backend.
// Caller must provide method and networkId to avoid parsing overhead in the exporter.
type misbehaviorExporter interface {
	AppendWithMetadata(line []byte, method string, networkId string) error
}

// file-based exporter implementation
type fileMisbehaviorExporter struct {
	mu  sync.Mutex
	cfg *common.MisbehaviorsDestinationConfig
	log *zerolog.Logger
	// Map of active file handles by resolved pattern
	fileHandles map[string]*os.File
}

func newFileMisbehaviorExporter(cfg *common.MisbehaviorsDestinationConfig, log *zerolog.Logger) (*fileMisbehaviorExporter, error) {
	if cfg == nil || cfg.Path == "" {
		return nil, fmt.Errorf("empty path configuration")
	}

	// Validate base path
	basePath := filepath.Clean(cfg.Path)
	if !filepath.IsAbs(basePath) {
		return nil, fmt.Errorf("export path must be absolute: %s", basePath)
	}

	// Create base directory
	if err := os.MkdirAll(basePath, 0o750); err != nil {
		return nil, err
	}

	return &fileMisbehaviorExporter{
		cfg:         cfg,
		log:         log,
		fileHandles: make(map[string]*os.File),
	}, nil
}

// AppendWithMetadata appends a line with contextual information for file naming
func (e *fileMisbehaviorExporter) AppendWithMetadata(line []byte, method string, networkId string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Resolve the file pattern with appropriate defaults
	fileName := resolveFilePatternWithDefaults(e.cfg, method, networkId, time.Now())
	fullPath := filepath.Join(e.cfg.Path, fileName)

	// Get or create file handle
	f, exists := e.fileHandles[fullPath]
	if !exists {
		var err error
		// #nosec G304 -- path is derived from admin-controlled configuration
		f, err = os.OpenFile(fullPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", fullPath, err)
		}
		e.fileHandles[fullPath] = f
	}

	if _, err := f.Write(line); err != nil {
		return err
	}
	_, err := f.Write([]byte("\n"))
	return err
}

// Note: No Append() without metadata; callers must pass method/networkId explicitly

func (e *fileMisbehaviorExporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var firstErr error
	for path, f := range e.fileHandles {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
			e.log.Warn().Err(err).Str("path", path).Msg("failed to close file handle")
		}
		delete(e.fileHandles, path)
	}
	return firstErr
}

// --- record shape ---

type misbehaviorRecord struct {
	TimestampMs  int64                 `json:"ts"`
	ProjectID    string                `json:"projectId"`
	NetworkID    string                `json:"networkId"`
	Method       string                `json:"method"`
	Finality     string                `json:"finality"`
	Policy       policySnapshot        `json:"policy"`
	Request      json.RawMessage       `json:"request"`
	Winner       winnerSnapshot        `json:"winner"`
	Analysis     analysisSnapshot      `json:"analysis"`
	Participants []participantSnapshot `json:"participants"`
}

type policySnapshot struct {
	MaxParticipants         int                                     `json:"maxParticipants"`
	AgreementThreshold      int                                     `json:"agreementThreshold"`
	DisputeBehavior         common.ConsensusDisputeBehavior         `json:"disputeBehavior"`
	LowParticipantsBehavior common.ConsensusLowParticipantsBehavior `json:"lowParticipantsBehavior"`
	PreferNonEmpty          bool                                    `json:"preferNonEmpty"`
	PreferLargerResponses   bool                                    `json:"preferLargerResponses"`
	IgnoreFields            map[string][]string                     `json:"ignoreFields,omitempty"`
}

type winnerSnapshot struct {
	ResponseType string `json:"responseType"`
	Hash         string `json:"hash"`
	Size         int    `json:"size"`
	UpstreamID   string `json:"upstreamId,omitempty"`
}

type analysisSnapshot struct {
	TotalParticipants int             `json:"totalParticipants"`
	ValidParticipants int             `json:"validParticipants"`
	BestByCount       *groupSnapshot  `json:"bestByCount,omitempty"`
	Groups            []groupSnapshot `json:"groups"`
}

type groupSnapshot struct {
	Hash         string `json:"hash"`
	Count        int    `json:"count"`
	IsTie        bool   `json:"isTie"`
	ResponseType string `json:"responseType"`
	ResponseSize int    `json:"responseSize"`
}

type participantSnapshot struct {
	UpstreamID   string `json:"upstreamId"`
	Vendor       string `json:"vendor,omitempty"`
	ResponseType string `json:"responseType"`
	ResponseHash string `json:"responseHash,omitempty"`
	ResponseSize int    `json:"responseSize,omitempty"`
	// One of the below is set
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
}
