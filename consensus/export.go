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
	Close() error
}

// sharedFileHubs owns the write lock and open handles for each absolute
// destination directory. Named policies that inherit the same file destination
// get independent fileMisbehaviorExporter wrappers (so each keeps its own
// policyName for `{policy}` substitution) but share one hub — concurrent
// appends cannot tear NDJSON lines across exporters.
var sharedFileHubs sync.Map // map[string]*sharedFileHub keyed by cleaned base path

type sharedFileHub struct {
	mu      sync.Mutex
	handles map[string]*os.File
	refs    int
}

func fileHubForBase(basePath string) *sharedFileHub {
	actual, _ := sharedFileHubs.LoadOrStore(basePath, &sharedFileHub{
		handles: make(map[string]*os.File),
	})
	return actual.(*sharedFileHub)
}

// file-based exporter implementation. Multiple instances may share a
// destination path; writes go through a sharedFileHub so they serialize.
// The hub pointer is pinned at construction. Hubs are retained in
// sharedFileHubs even at refs==0 so a concurrent constructor cannot
// LoadOrStore a second hub for the same path after a last Close.
type fileMisbehaviorExporter struct {
	cfg        *common.MisbehaviorsDestinationConfig
	log        *zerolog.Logger
	policyName string
	basePath   string
	hub        *sharedFileHub
	closed     bool
}

func newFileMisbehaviorExporter(cfg *common.MisbehaviorsDestinationConfig, log *zerolog.Logger, policyName string) (*fileMisbehaviorExporter, error) {
	if cfg == nil || cfg.Path == "" {
		return nil, fmt.Errorf("empty path configuration")
	}

	basePath := filepath.Clean(cfg.Path)
	if !filepath.IsAbs(basePath) {
		return nil, fmt.Errorf("export path must be absolute: %s", basePath)
	}

	if err := os.MkdirAll(basePath, 0o750); err != nil {
		return nil, err
	}

	hub := fileHubForBase(basePath)
	hub.mu.Lock()
	hub.refs++
	hub.mu.Unlock()

	return &fileMisbehaviorExporter{
		cfg:        cfg,
		log:        log,
		policyName: policyName,
		basePath:   basePath,
		hub:        hub,
	}, nil
}

// AppendWithMetadata appends a line with contextual information for file naming.
// The closed check and the framed write share hub.mu so Close cannot race a
// write onto a hub that was already deleted from sharedFileHubs.
func (e *fileMisbehaviorExporter) AppendWithMetadata(line []byte, method string, networkId string) error {
	fileName := resolveFilePatternWithDefaults(e.cfg, method, networkId, time.Now(), e.policyName)
	fullPath := filepath.Join(e.basePath, fileName)

	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	if e.closed {
		// Re-attach on append-after-close: "close flushes every exporter; a
		// later catch reopens its file" (integrity export path). The hub is
		// retained in sharedFileHubs at refs==0, so re-incrementing here keeps
		// the refcount consistent with the matching Close.
		e.hub.refs++
		e.closed = false
	}

	f, exists := e.hub.handles[fullPath]
	if !exists {
		// Default file patterns include {timestampMs}, so each flush can resolve
		// a new path. Evict prior handles for this destination so a long-lived
		// process cannot accumulate one FD per flush.
		for p, old := range e.hub.handles {
			_ = old.Close()
			delete(e.hub.handles, p)
		}
		var err error
		// #nosec G304 -- path is derived from admin-controlled configuration
		f, err = os.OpenFile(fullPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", fullPath, err)
		}
		e.hub.handles[fullPath] = f
	}

	if _, err := f.Write(append(append([]byte{}, line...), '\n')); err != nil {
		return err
	}
	return nil
}

// Close drops this exporter's reference on the shared hub. When the last
// exporter for a destination path closes, all cached descriptors are closed.
// The hub itself stays in sharedFileHubs at refs==0 so a concurrent
// constructor always finds the same lock — deleting it would let LoadOrStore
// create a second hub and split NDJSON writes across two mutexes.
func (e *fileMisbehaviorExporter) Close() error {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.hub.refs > 0 {
		e.hub.refs--
	}
	if e.hub.refs > 0 {
		return nil
	}
	var firstErr error
	for p, f := range e.hub.handles {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
			if e.log != nil {
				e.log.Warn().Err(err).Str("path", p).Msg("failed to close misbehavior file handle")
			}
		}
		delete(e.hub.handles, p)
	}
	return firstErr
}

// --- record shape ---

type misbehaviorRecord struct {
	TimestampMs  int64                 `json:"ts"`
	ProjectID    string                `json:"projectId"`
	UserId       string                `json:"userId,omitempty"`
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

// MisbehaviorExporter is the exported view of the JSONL exporter so other
// subsystems (e.g. the data-integrity module's misbehaviorsDestination) can
// reuse the same file/S3 machinery and config shape.
type MisbehaviorExporter = misbehaviorExporter

// NewMisbehaviorExporter builds the configured exporter. nil when the config
// is unset or initialization fails — export is best-effort by design.
func NewMisbehaviorExporter(cfg *common.MisbehaviorsDestinationConfig, log *zerolog.Logger) MisbehaviorExporter {
	return createMisbehaviorExporter(cfg, log, defaultPolicyName)
}
