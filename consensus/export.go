package consensus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// misbehaviorExporter appends encoded JSONL lines to a storage backend.
type misbehaviorExporter interface {
	Append(line []byte) error
	Close() error
}

// file-based exporter implementation
type fileMisbehaviorExporter struct {
	mu   sync.Mutex
	f    *os.File
	path string
	log  *zerolog.Logger
}

func newFileMisbehaviorExporter(path string, log *zerolog.Logger) (*fileMisbehaviorExporter, error) {
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}
	// Clean and validate to avoid unsafe relative traversal
	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		return nil, fmt.Errorf("export path must be absolute: %s", cleanPath)
	}
	// Create directory tree with restrictive permissions
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o750); err != nil {
		return nil, err
	}
	// Open file with restrictive permissions (0600). Path validated above.
	// #nosec G304 -- path is cleaned and must be absolute; this is operator-provided
	f, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileMisbehaviorExporter{f: f, path: cleanPath, log: log}, nil
}

func (e *fileMisbehaviorExporter) Append(line []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return fmt.Errorf("exporter closed")
	}
	if _, err := e.f.Write(line); err != nil {
		return err
	}
	_, err := e.f.Write([]byte("\n"))
	return err
}

func (e *fileMisbehaviorExporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
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
