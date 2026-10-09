// Package history writes versioned request history to owner-only, rotating NDJSON files.
package history

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	schemaVersion         = 2
	minimumSchemaVersion  = 1
	maxEventBytes         = 64 << 20
	maxRequestID          = 128
	maxSafeError          = 8 << 10
	maxActiveBytes        = 64 << 20
	activeRotationAge     = time.Hour
	rotationCheckInterval = time.Minute
	activeFileName        = "active.ndjson"
	closedFilePrefix      = "segment-"
	closedFileSuffix      = ".ndjson"
	closedTimestampLayout = "20060102T150405.000000000Z"
)

var (
	// ErrRecordingUnavailable is returned when the history sink cannot accept a record.
	ErrRecordingUnavailable = errors.New("request recording is unavailable")
	// ErrEventTooLarge rejects one oversized SSE frame without degrading the recorder.
	ErrEventTooLarge = errors.New("response event exceeds the 64 MiB recording limit")
	// ErrInvalidRecord reports caller data that cannot be represented by this schema.
	ErrInvalidRecord = errors.New("invalid request history record")
	// ErrRequestEnded rejects operations after a request has been finalized.
	ErrRequestEnded = errors.New("request history handle has ended")
)

// ServiceAccountSnapshot is the non-secret admission-time identity written to history.
type ServiceAccountSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ModelSnapshot is the admission-time named model and routing configuration version.
type ModelSnapshot struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Strategy string `json:"strategy"`
}

// Start contains the non-secret snapshots needed to attribute an admitted request.
// IncomingRequest must be the complete request JSON after removing managed HTTP
// authentication fields; arbitrary user-provided request content is preserved.
type Start struct {
	RequestID       string
	ServiceAccount  ServiceAccountSnapshot
	Model           ModelSnapshot
	IncomingRequest json.RawMessage
}

// ProviderSnapshot identifies the actual provider target used for one upstream attempt.
type ProviderSnapshot struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UpstreamModel string `json:"upstream_model"`
}

// Attempt is the target and effective request body for one upstream transmission.
type Attempt struct {
	Number           int
	Provider         ProviderSnapshot
	EffectiveRequest json.RawMessage
}

// Usage stores provider-reported token counts. A nil count means unknown; a pointer
// to zero means the provider reported zero. Cached/reasoning counts are subsets.
type Usage struct {
	InputTokens           *int64 `json:"input_tokens"`
	OutputTokens          *int64 `json:"output_tokens"`
	CachedInputTokens     *int64 `json:"cached_input_tokens"`
	ReasoningOutputTokens *int64 `json:"reasoning_output_tokens"`
}

// Timings contains monotonic nanosecond offsets from request admission. Nil fields
// mean that a stage was not observed, rather than that it took zero time.
type Timings struct {
	AuthenticationPreparationNS *int64 `json:"authentication_preparation_ns"`
	ConnectionRequestedNS       *int64 `json:"connection_requested_ns,omitempty"`
	ConnectionEstablishedNS     *int64 `json:"connection_established_ns"`
	RequestTransmissionNS       *int64 `json:"request_transmission_ns"`
	FirstEventNS                *int64 `json:"first_event_ns"`
	FirstOutputNS               *int64 `json:"first_output_ns"`
	StreamCompletionNS          *int64 `json:"stream_completion_ns"`
	DownstreamDeliveryNS        *int64 `json:"downstream_delivery_ns"`
	ConnectionReused            *bool  `json:"connection_reused,omitempty"`
}

// RecorderStatus contains only safe operational counters and state.
type RecorderStatus struct {
	State                  string `json:"state"`
	PotentiallyLostRecords uint64 `json:"potentiallyLostRecords"`
	RecoveredBytes         uint64 `json:"recoveredBytes"`
	ActiveRequests         int    `json:"activeRequests"`
	BytesWritten           int64  `json:"bytesWritten"`
}

// ClosedSegment is the safe metadata snapshot for one immutable, uncompressed
// history segment. Name is a single filename, never a filesystem path.
type ClosedSegment struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// Recorder serializes every record, durability fence, and active-file rotation under
// one mutex so records from concurrent requests cannot interleave. A storage error
// permanently degrades this process's recorder.
type Recorder struct {
	mu                                  sync.Mutex
	file                                *os.File
	historyDir                          string
	segmentStartedAt                    time.Time
	active                              map[string]*Request
	state                               string
	failure                             error
	closed                              bool
	closing                             bool
	closeDone                           chan struct{}
	potentiallyLostRecords              uint64
	recoveredBytes                      uint64
	bytesWritten                        int64
	unsyncedRecords                     uint64
	closeErr                            error
	rotationStop                        chan struct{}
	rotationDone                        chan struct{}
	rotationStarted                     bool
	rotationStopOnce                    sync.Once
	queryGate                           *operationGate
	zstdExecutable                      string
	compressionCtx                      context.Context
	compressionCancel                   context.CancelFunc
	verifiedCompressed                  map[string]bool
	compressionFailures                 map[string]bool
	compressionLastError                string
	compressionLastSegment              string
	compressionLastAt                   time.Time
	compressedQueryErr                  error
	deletionState                       string
	deletionError                       string
	deletionFrom                        *time.Time
	deletionTo                          *time.Time
	deletionCompletedSegments           int
	deletionTotalSegments               int
	deletionUpdatedAt                   time.Time
	pendingMaintenance                  *MaintenanceLease
	pendingDeletionEntries              map[string]bool
	deletionCompressedValidationPending bool
	deletionServiceCancel               context.CancelFunc
	deletionServiceDone                 chan struct{}
}

// Request is a handle for one admitted inference request. It is safe for concurrent
// calls; End is idempotent and unregisters the request even if recording has failed.
type Request struct {
	recorder       *Recorder
	id             string
	startedAt      time.Time
	monotonicStart time.Time
	serviceAccount ServiceAccountSnapshot
	model          ModelSnapshot
	lastProvider   *ProviderSnapshot
	ended          bool
	lastAttempt    int
	eventSequence  uint64
}

// Open prepares dataDir/history/active.ndjson. On filesystem or recovery failure it
// returns a degraded recorder and a diagnostic error; callers should keep the UI
// running and pass the recorder to the UI and gateway admission path. Existing data
// is validated before append, and only an incomplete active-file tail is truncated.
func Open(dataDir string) (*Recorder, error) {
	r := &Recorder{
		active:              make(map[string]*Request),
		closeDone:           make(chan struct{}),
		state:               "degraded",
		queryGate:           newOperationGate(),
		verifiedCompressed:  make(map[string]bool),
		compressionFailures: make(map[string]bool),
		deletionState:       "idle",
	}
	if strings.TrimSpace(dataDir) == "" {
		return r, r.degrade(errors.New("history data directory is empty"))
	}
	dir := filepath.Join(dataDir, "history")
	_, dirStatErr := os.Stat(dir)
	historyDirCreated := errors.Is(dirStatErr, os.ErrNotExist)
	if dirStatErr != nil && !historyDirCreated {
		return r, r.degrade(fmt.Errorf("inspect history directory: %w", dirStatErr))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return r, r.degrade(fmt.Errorf("create history directory: %w", err))
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return r, r.degrade(fmt.Errorf("inspect history directory: %w", err))
	}
	if !dirInfo.IsDir() {
		return r, r.degrade(errors.New("history path is not a directory"))
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return r, r.degrade(fmt.Errorf("set history directory permissions: %w", err))
	}
	if historyDirCreated {
		if err := syncDirectory(dataDir); err != nil {
			return r, r.degrade(fmt.Errorf("sync parent of history directory: %w", err))
		}
	}
	r.historyDir = dir
	r.zstdExecutable = resolveZstdExecutable()
	pendingDeletion := r.recoverDeletionAtStartup()
	if !pendingDeletion {
		if err := cleanupCompressionTemps(dir); err != nil {
			r.noteCompressionError("temporary compression files could not be cleaned up", "")
		}

		segments, err := listClosedSegments(dir)
		if err != nil {
			return r, r.degrade(fmt.Errorf("list closed history segments: %w", err))
		}
		compressed, err := listCompressedSegments(dir)
		if err != nil {
			return r, r.degrade(fmt.Errorf("list compressed history segments: %w", err))
		}
		for _, segment := range segments {
			path := filepath.Join(dir, segment.Name)
			if err := validateHistoryFile(path, false, nil); err != nil {
				return r, r.degrade(fmt.Errorf("closed history segment %q is invalid: %w", segment.Name, err))
			}
		}
		if len(compressed) > 0 && r.zstdExecutable == "" {
			r.compressedQueryErr = ErrZstdUnavailable
			r.noteCompressionError("compressed history cannot be validated because zstd is unavailable", "")
		}
		for _, segment := range compressed {
			compressedPath := filepath.Join(dir, segment.Name)
			if r.zstdExecutable == "" {
				continue
			}
			digest, length, validationErr := validateCompressedHistory(context.Background(), r.zstdExecutable, compressedPath, nil)
			if validationErr != nil {
				r.compressedQueryErr = fmt.Errorf("%w: segment %q could not be validated", ErrCompressedHistoryInvalid, segment.Name)
				r.compressionFailures[segment.Name] = true
				r.noteCompressionError("compressed history failed startup validation", segment.Name)
				return r, r.degrade(fmt.Errorf("compressed history segment %q is invalid: %w", segment.Name, validationErr))
			}
			if err := syncCompressedFile(compressedPath); err != nil {
				r.compressedQueryErr = fmt.Errorf("%w: segment %q could not be synced", ErrCompressedHistoryInvalid, segment.Name)
				return r, r.degrade(fmt.Errorf("sync validated compressed history segment %q: %w", segment.Name, err))
			}
			rawName := strings.TrimSuffix(segment.Name, compressedSuffix)
			rawPath := filepath.Join(dir, rawName)
			if _, statErr := os.Lstat(rawPath); errors.Is(statErr, os.ErrNotExist) {
				continue
			} else if statErr != nil {
				return r, r.degrade(fmt.Errorf("inspect raw source for compressed segment %q: %w", segment.Name, statErr))
			}
			rawDigest, rawLength, hashErr := hashRegularFile(context.Background(), rawPath)
			if hashErr != nil {
				r.compressedQueryErr = fmt.Errorf("%w: pair %q could not be compared", ErrCompressedHistoryInvalid, rawName)
				return r, r.degrade(fmt.Errorf("verify raw and compressed history pair %q: %w", segment.Name, hashErr))
			}
			if length != rawLength || digest != rawDigest {
				r.compressedQueryErr = fmt.Errorf("%w: pair %q does not match", ErrCompressedHistoryInvalid, rawName)
				r.compressionFailures[segment.Name] = true
				r.noteCompressionError("raw and compressed history copies do not match", rawName)
				return r, r.degrade(fmt.Errorf("raw and compressed history segment pair %q does not match", rawName))
			}
			r.verifiedCompressed[rawName] = true
			if err := syncDirectory(dir); err != nil {
				return r, r.degrade(fmt.Errorf("sync history directory before raw source removal: %w", err))
			}
			if err := os.Remove(rawPath); err != nil {
				r.compressionFailures[segment.Name] = true
				r.noteCompressionError("verified raw source could not be removed", rawName)
				continue
			}
			if err := syncDirectory(dir); err != nil {
				r.compressionFailures[segment.Name] = true
				r.noteCompressionError("history directory could not be synced after source removal", rawName)
			} else {
				delete(r.verifiedCompressed, rawName)
			}
		}
	} else {
		if err := r.validateUntouchedHistory(); err != nil {
			return r, r.degrade(err)
		}
	}

	activePath := filepath.Join(dir, activeFileName)
	activeInfo, activeStatErr := os.Lstat(activePath)
	activeExists := activeStatErr == nil
	if activeStatErr != nil && !errors.Is(activeStatErr, os.ErrNotExist) {
		return r, r.degrade(fmt.Errorf("inspect active history file: %w", activeStatErr))
	}
	var scan scanResult
	if activeExists {
		if !activeInfo.Mode().IsRegular() {
			return r, r.degrade(errors.New("active history path is not a regular file"))
		}
		activeFile, err := os.OpenFile(activePath, os.O_RDWR, 0)
		if err != nil {
			return r, r.degrade(fmt.Errorf("open active history file for recovery: %w", err))
		}
		activeInfo, err = activeFile.Stat()
		if err != nil {
			_ = activeFile.Close()
			return r, r.degrade(fmt.Errorf("inspect active history file: %w", err))
		}
		if !activeInfo.Mode().IsRegular() {
			_ = activeFile.Close()
			return r, r.degrade(errors.New("active history path is not a regular file"))
		}
		scan, err = scanHistoryFile(activeFile, true)
		if err != nil {
			_ = activeFile.Close()
			return r, r.degrade(fmt.Errorf("active history file is invalid: %w", err))
		}
		if scan.TailBytes > 0 {
			truncateTo := activeInfo.Size() - scan.TailBytes
			if truncateTo < 0 {
				_ = activeFile.Close()
				return r, r.degrade(errors.New("active history recovery offset is invalid"))
			}
			if err := activeFile.Truncate(truncateTo); err != nil {
				_ = activeFile.Close()
				return r, r.degrade(fmt.Errorf("truncate incomplete active history tail: %w", err))
			}
			if err := activeFile.Sync(); err != nil {
				_ = activeFile.Close()
				return r, r.degrade(fmt.Errorf("sync recovered active history file: %w", err))
			}
			r.recoveredBytes = uint64(scan.TailBytes)
			activeInfo, err = activeFile.Stat()
			if err != nil {
				_ = activeFile.Close()
				return r, r.degrade(fmt.Errorf("inspect recovered active history file: %w", err))
			}
		}
		if err := activeFile.Close(); err != nil {
			return r, r.degrade(fmt.Errorf("close active history file after recovery: %w", err))
		}
	} else {
		activeInfo = nil
	}

	var file *os.File
	if activeExists {
		file, err = os.OpenFile(activePath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return r, r.degrade(fmt.Errorf("open active history file: %w", err))
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return r, r.degrade(fmt.Errorf("set active history file permissions: %w", err))
		}
		activeInfo, err = file.Stat()
		if err != nil {
			_ = file.Close()
			return r, r.degrade(fmt.Errorf("inspect active history file: %w", err))
		}
	} else {
		file, err = os.OpenFile(activePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return r, r.degrade(fmt.Errorf("create active history file: %w", err))
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return r, r.degrade(fmt.Errorf("set active history file permissions: %w", err))
		}
		if err := syncDirectory(dir); err != nil {
			_ = file.Close()
			return r, r.degrade(fmt.Errorf("sync history directory after active file creation: %w", err))
		}
		activeInfo, err = file.Stat()
		if err != nil {
			_ = file.Close()
			return r, r.degrade(fmt.Errorf("inspect active history file: %w", err))
		}
	}
	if !activeInfo.Mode().IsRegular() {
		_ = file.Close()
		return r, r.degrade(errors.New("active history path is not a regular file"))
	}
	r.file = file
	r.bytesWritten = activeInfo.Size()
	r.segmentStartedAt = activeInfo.ModTime()
	if activeInfo.Size() == 0 {
		r.segmentStartedAt = time.Now()
	}
	if scan.Records > 0 {
		r.segmentStartedAt = scan.FirstStartedAt
	}
	if r.segmentStartedAt.IsZero() {
		r.segmentStartedAt = time.Now()
	}
	r.state = "ready"

	if r.bytesWritten > 0 {
		r.mu.Lock()
		err := r.rotateLocked(time.Now())
		r.mu.Unlock()
		if err != nil {
			return r, fmt.Errorf("rotate recovered active history file: %w", err)
		}
	}
	r.compressionCtx, r.compressionCancel = context.WithCancel(context.Background())
	r.startRotation()
	return r, nil
}

func (r *Recorder) degrade(err error) error {
	r.failure = err
	r.state = "degraded"
	return err
}

func validateHistoryFile(path string, allowIncompleteTail bool, visit func(Record) error) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = walkRecords(context.Background(), file, allowIncompleteTail, visitOrIgnore(visit))
	return err
}

func visitOrIgnore(visit func(Record) error) func(Record) error {
	if visit == nil {
		return func(Record) error { return nil }
	}
	return visit
}

func scanHistoryFile(file *os.File, allowIncompleteTail bool) (scanResult, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return scanResult{}, err
	}
	return walkRecords(context.Background(), file, allowIncompleteTail, func(Record) error { return nil })
}

func listClosedSegments(dir string) ([]ClosedSegment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	segments := make([]ClosedSegment, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, closedFilePrefix) || !strings.HasSuffix(name, closedFileSuffix) {
			continue
		}
		if !validClosedFileName(name) {
			return nil, fmt.Errorf("unrecognized closed history filename %q", name)
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("closed history segment %q is not a regular file", name)
		}
		segments = append(segments, ClosedSegment{
			Name:       name,
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Name < segments[j].Name })
	return segments, nil
}

func validClosedFileName(name string) bool {
	if !strings.HasPrefix(name, closedFilePrefix) || !strings.HasSuffix(name, closedFileSuffix) {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, closedFilePrefix), closedFileSuffix)
	separator := strings.LastIndexByte(stem, '-')
	if separator < 0 {
		return false
	}
	if _, err := time.Parse(closedTimestampLayout, stem[:separator]); err != nil {
		return false
	}
	suffix, err := hex.DecodeString(stem[separator+1:])
	return err == nil && len(suffix) == 16
}

func closedFileName(startedAt time.Time) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return closedFilePrefix + startedAt.UTC().Format(closedTimestampLayout) + "-" + hex.EncodeToString(suffix[:]) + closedFileSuffix, nil
}

func (r *Recorder) rotationDue(now time.Time) bool {
	return r.bytesWritten >= maxActiveBytes || (!r.segmentStartedAt.IsZero() && now.Sub(r.segmentStartedAt) >= activeRotationAge)
}

func (r *Recorder) startRotation() {
	r.rotationStop = make(chan struct{})
	r.rotationDone = make(chan struct{})
	r.rotationStarted = true
	go func() {
		defer close(r.rotationDone)
		ticker := time.NewTicker(rotationCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.mu.Lock()
				if !r.closed && !r.closing && r.failure == nil && r.file != nil && r.bytesWritten > 0 && r.rotationDue(time.Now()) {
					_ = r.rotateLocked(time.Now())
				}
				r.mu.Unlock()
				r.compressPending(r.compressionCtx)
			case <-r.rotationStop:
				return
			}
		}
	}()
}

func (r *Recorder) stopRotation() {
	if !r.rotationStarted {
		return
	}
	r.rotationStopOnce.Do(func() {
		close(r.rotationStop)
		if r.compressionCancel != nil {
			r.compressionCancel()
		}
	})
	<-r.rotationDone
}

func (r *Recorder) rotateLocked(now time.Time) error {
	if r.closed || r.file == nil || r.failure != nil {
		return ErrRecordingUnavailable
	}
	if err := r.file.Sync(); err != nil {
		r.markSyncFailureLocked(err)
		return fmt.Errorf("sync active history before rotation: %w", err)
	}
	r.unsyncedRecords = 0
	if err := r.file.Close(); err != nil {
		r.file = nil
		r.failure = err
		r.state = "degraded"
		return fmt.Errorf("close active history before rotation: %w", err)
	}
	r.file = nil

	var closedPath string
	for attempt := 0; attempt < 4; attempt++ {
		name, err := closedFileName(r.segmentStartedAt)
		if err != nil {
			r.failure = err
			r.state = "degraded"
			return fmt.Errorf("create closed history filename: %w", err)
		}
		candidate := filepath.Join(r.historyDir, name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			closedPath = candidate
			break
		} else if err != nil {
			r.failure = err
			r.state = "degraded"
			return fmt.Errorf("inspect closed history filename: %w", err)
		}
	}
	if closedPath == "" {
		err := errors.New("could not allocate a unique closed history filename")
		r.failure = err
		r.state = "degraded"
		return err
	}
	activePath := filepath.Join(r.historyDir, activeFileName)
	if err := os.Rename(activePath, closedPath); err != nil {
		r.failure = err
		r.state = "degraded"
		return fmt.Errorf("publish closed history segment: %w", err)
	}
	if err := syncDirectory(r.historyDir); err != nil {
		r.failure = err
		r.state = "degraded"
		return fmt.Errorf("sync history directory after segment publication: %w", err)
	}
	newFile, err := os.OpenFile(activePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.failure = err
		r.state = "degraded"
		return fmt.Errorf("create fresh active history file: %w", err)
	}
	if err := newFile.Chmod(0o600); err != nil {
		_ = newFile.Close()
		r.failure = err
		r.state = "degraded"
		return fmt.Errorf("set fresh active history permissions: %w", err)
	}
	if err := syncDirectory(r.historyDir); err != nil {
		_ = newFile.Close()
		r.failure = err
		r.state = "degraded"
		return fmt.Errorf("sync history directory after active file creation: %w", err)
	}
	r.file = newFile
	r.bytesWritten = 0
	r.segmentStartedAt = now
	return nil
}

// ClosedSegments returns a sorted metadata snapshot of closed raw NDJSON segments.
// Segment names are stable and unique; the active file is deliberately omitted.
func (r *Recorder) ClosedSegments() ([]ClosedSegment, error) {
	if r == nil || r.historyDir == "" {
		return nil, ErrRecordingUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return listClosedSegments(r.historyDir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Begin durably records the admission snapshot before returning a handle. A caller
// must obtain a successful handle before invoking any upstream provider.
func (r *Recorder) Begin(start Start) (*Request, error) {
	if r == nil {
		return nil, ErrRecordingUnavailable
	}
	if start.RequestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("create history request id: %w", err)
		}
		start.RequestID = id
	}
	if !validRequestID(start.RequestID) || strings.TrimSpace(start.ServiceAccount.ID) == "" || strings.TrimSpace(start.ServiceAccount.Name) == "" ||
		strings.TrimSpace(start.Model.ID) == "" || strings.TrimSpace(start.Model.Name) == "" || start.Model.Version < 1 ||
		strings.TrimSpace(start.Model.Strategy) == "" || len(start.IncomingRequest) == 0 || !validJSONObject(start.IncomingRequest) {
		return nil, fmt.Errorf("%w: admission snapshot is incomplete", ErrInvalidRecord)
	}
	start.IncomingRequest = append(json.RawMessage(nil), start.IncomingRequest...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || r.closed {
		return nil, ErrRecordingUnavailable
	}
	if _, exists := r.active[start.RequestID]; exists {
		return nil, fmt.Errorf("%w: request id is already active", ErrInvalidRecord)
	}
	now := time.Now()
	handle := &Request{
		recorder:       r,
		id:             start.RequestID,
		startedAt:      now.UTC(),
		monotonicStart: now,
		serviceAccount: start.ServiceAccount,
		model:          start.Model,
	}
	line := startLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "request_start",
			RequestID:     handle.id,
			StartedAt:     handle.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     0,
		},
		ServiceAccount:  start.ServiceAccount,
		Model:           start.Model,
		IncomingRequest: start.IncomingRequest,
	}
	if err := r.writeLocked(line, true); err != nil {
		return nil, err
	}
	r.active[handle.id] = handle
	return handle, nil
}

// ID returns the stable gateway request ID associated with this handle.
func (h *Request) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

// StartedAt returns the UTC wall-clock admission timestamp shared by every record.
func (h *Request) StartedAt() time.Time {
	if h == nil {
		return time.Time{}
	}
	return h.startedAt
}

// ElapsedNS returns the request's monotonic nanosecond offset from admission.
func (h *Request) ElapsedNS() int64 {
	if h == nil {
		return 0
	}
	return h.elapsedNS(time.Now())
}

// Transmit durably records an attempt before the caller transmits its effective
// request upstream. Attempt numbers start at one and increase for each request.
func (h *Request) Transmit(attempt Attempt) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	if attempt.Number < 1 || strings.TrimSpace(attempt.Provider.ID) == "" || strings.TrimSpace(attempt.Provider.Name) == "" ||
		strings.TrimSpace(attempt.Provider.UpstreamModel) == "" || len(attempt.EffectiveRequest) == 0 || !validJSONObject(attempt.EffectiveRequest) {
		return fmt.Errorf("%w: transmission snapshot is incomplete", ErrInvalidRecord)
	}
	attempt.EffectiveRequest = append(json.RawMessage(nil), attempt.EffectiveRequest...)
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return ErrRequestEnded
	}
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	if attempt.Number <= h.lastAttempt {
		return fmt.Errorf("%w: attempt number must increase", ErrInvalidRecord)
	}
	now := time.Now()
	line := transmitLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "upstream_transmission",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt:          attempt.Number,
		ProviderID:       attempt.Provider.ID,
		ProviderName:     attempt.Provider.Name,
		UpstreamModel:    attempt.Provider.UpstreamModel,
		EffectiveRequest: attempt.EffectiveRequest,
	}
	err := r.writeLocked(line, true)
	if err == nil {
		h.lastAttempt = attempt.Number
		provider := attempt.Provider
		h.lastProvider = &provider
	}
	return err
}

// Event records one complete upstream SSE frame immediately. Sequence numbers are
// assigned by the handle, beginning at one; an event is limited individually, so
// the total stream may exceed 64 MiB.
func (h *Request) Event(frame []byte, eventName string) error {
	if len(frame) > maxEventBytes {
		return ErrEventTooLarge
	}
	if !utf8.Valid(frame) {
		return fmt.Errorf("%w: SSE frame is not UTF-8 text", ErrInvalidRecord)
	}
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return ErrRequestEnded
	}
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	if h.lastAttempt == 0 {
		return fmt.Errorf("%w: response event has no recorded transmission", ErrInvalidRecord)
	}
	h.eventSequence++
	now := time.Now()
	line := eventLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "response_event",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt:   h.lastAttempt,
		EventName: eventName,
		Sequence:  h.eventSequence,
		WireText:  string(frame),
	}
	return r.writeLocked(line, false)
}

// EndDetails contains small, safe upstream metadata for one request summary.
// Error bodies belong in an upstream_response record instead. DroppedFields is
// the classified drop list from request preparation: removed field names plus
// "<container>.<tool-type>" entries for removed tool definitions.
type EndDetails struct {
	UpstreamRequestID string
	DroppedFields     []string
}

// validDroppedFields bounds the drop list: a small number of short field names.
// The list is gateway-generated; the bounds keep a hostile end record from
// inflating the file, and empty entries carry no information.
func validDroppedFields(fields []string) bool {
	if len(fields) > 64 {
		return false
	}
	for _, field := range fields {
		if len(field) == 0 || len(field) > 128 || !utf8.ValidString(field) {
			return false
		}
	}
	return true
}

// End records a final request outcome and always unregisters the handle. Outcomes
// are completed, failed, cancelled, or incomplete. safeError must be a short,
// pre-sanitized explanation and must not contain bodies, credentials, or URLs.
func (h *Request) End(outcome string, httpStatus int, safeError string, usage *Usage, timings Timings) error {
	return h.EndWithDetails(outcome, httpStatus, safeError, usage, timings, EndDetails{})
}

// EndWithDetails adds a safe provider request identifier while keeping the end
// record self-contained for later attribution scans.
func (h *Request) EndWithDetails(outcome string, httpStatus int, safeError string, usage *Usage, timings Timings, details EndDetails) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return nil
	}
	if !validUpstreamRequestID(details.UpstreamRequestID) || !validOutcome(outcome) || httpStatus < 0 || httpStatus > 599 || len(safeError) > maxSafeError || !utf8.ValidString(safeError) || !validTimings(timings) || !validUsage(usage) || !validDroppedFields(details.DroppedFields) {
		writeErr := r.writeEndLocked(h, "incomplete", 0, "request ended with invalid final details", nil, Timings{}, EndDetails{})
		h.ended = true
		delete(r.active, h.id)
		if writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("%w: invalid end details", ErrInvalidRecord)
	}
	if usage != nil {
		usage = cloneUsage(usage)
	}
	timings = cloneTimings(timings)
	err := r.writeEndLocked(h, outcome, httpStatus, safeError, usage, timings, details)
	h.ended = true
	delete(r.active, h.id)
	return err
}

func (r *Recorder) writeEndLocked(h *Request, outcome string, httpStatus int, safeError string, usage *Usage, timings Timings, details EndDetails) error {
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	now := time.Now()
	line := endLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "request_end",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt:           h.lastAttempt,
		Outcome:           outcome,
		HTTPStatus:        httpStatus,
		SafeError:         safeError,
		Usage:             usage,
		Timings:           timings,
		ServiceAccount:    h.serviceAccount,
		Model:             h.model,
		Provider:          h.lastProvider,
		UpstreamRequestID: details.UpstreamRequestID,
		DroppedFields:     details.DroppedFields,
	}
	return r.writeLocked(line, true)
}

// HTTPResponse stores a bounded upstream HTTP error/non-SSE body as exact bytes.
// contentType is the sanitized upstream Content-Type header value, empty when the
// upstream sent none. It is a non-durable record; the following End is the
// durability fence.
func (h *Request) HTTPResponse(status int, contentType, upstreamRequestID string, body []byte, truncated, readFailed bool) error {
	if h == nil || h.recorder == nil {
		return ErrRecordingUnavailable
	}
	if status < 100 || status > 599 || !validContentTypeHeader(contentType) || !validUpstreamRequestID(upstreamRequestID) || len(body) > maxEventBytes {
		return fmt.Errorf("%w: upstream response snapshot is invalid", ErrInvalidRecord)
	}
	r := h.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ended {
		return ErrRequestEnded
	}
	if r.active[h.id] != h {
		return ErrRecordingUnavailable
	}
	if h.lastAttempt == 0 {
		return fmt.Errorf("%w: upstream response has no recorded transmission", ErrInvalidRecord)
	}
	now := time.Now()
	line := upstreamResponseLine{
		baseLine: baseLine{
			SchemaVersion: schemaVersion,
			Type:          "upstream_response",
			RequestID:     h.id,
			StartedAt:     h.startedAt,
			TimestampUTC:  now.UTC(),
			ElapsedNS:     h.elapsedNS(now),
		},
		Attempt: h.lastAttempt, HTTPStatus: status, ContentType: contentType, UpstreamRequestID: upstreamRequestID,
		Body: append([]byte(nil), body...), BodyTruncated: truncated, BodyReadFailed: readFailed,
	}
	return r.writeLocked(line, false)
}

func (h *Request) elapsedNS(now time.Time) int64 {
	elapsed := now.Sub(h.monotonicStart).Nanoseconds()
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func (r *Recorder) writeLocked(record any, durable bool) error {
	if r.closed || r.file == nil || r.failure != nil {
		r.potentiallyLostRecords++
		return ErrRecordingUnavailable
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode request history record: %w", err)
	}
	if r.bytesWritten > 0 && r.rotationDue(time.Now()) {
		if err := r.rotateLocked(time.Now()); err != nil {
			r.potentiallyLostRecords++
			return ErrRecordingUnavailable
		}
	}
	encoded = append(encoded, '\n')
	remaining := encoded
	for len(remaining) > 0 {
		n, writeErr := r.file.Write(remaining)
		if n < 0 || n > len(remaining) {
			r.markWriteFailureLocked(errors.New("history writer returned an invalid byte count"))
			return ErrRecordingUnavailable
		}
		r.bytesWritten += int64(n)
		remaining = remaining[n:]
		if writeErr != nil {
			r.markWriteFailureLocked(writeErr)
			return ErrRecordingUnavailable
		}
		if n == 0 {
			r.markWriteFailureLocked(io.ErrShortWrite)
			return ErrRecordingUnavailable
		}
	}
	r.unsyncedRecords++
	if !durable {
		return nil
	}
	if err := r.file.Sync(); err != nil {
		r.markSyncFailureLocked(err)
		return ErrRecordingUnavailable
	}
	r.unsyncedRecords = 0
	return nil
}

func (r *Recorder) markWriteFailureLocked(err error) {
	r.failure = err
	r.state = "degraded"
	r.potentiallyLostRecords += r.unsyncedRecords + 1
	r.unsyncedRecords = 0
}

func (r *Recorder) markSyncFailureLocked(err error) {
	r.failure = err
	r.state = "degraded"
	r.potentiallyLostRecords += r.unsyncedRecords
	r.unsyncedRecords = 0
}

// Status returns storage health without returning error strings, paths, or request data.
func (r *Recorder) Status() RecorderStatus {
	if r == nil {
		return RecorderStatus{State: "degraded"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.state
	if r.closed {
		state = "closed"
	}
	return RecorderStatus{
		State:                  state,
		PotentiallyLostRecords: r.potentiallyLostRecords,
		RecoveredBytes:         r.recoveredBytes,
		ActiveRequests:         len(r.active),
		BytesWritten:           r.bytesWritten,
	}
}

// Close completes any still-active requests as incomplete, syncs pending event
// records, and closes the active file. It is safe to call more than once.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	if r.closing {
		done := r.closeDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closing = true
	deletionCancel := r.deletionServiceCancel
	deletionDone := r.deletionServiceDone
	r.mu.Unlock()
	r.stopRotation()
	if deletionCancel != nil {
		deletionCancel()
		if deletionDone != nil {
			<-deletionDone
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if r.closeDone != nil {
			close(r.closeDone)
			r.closeDone = nil
		}
	}()
	if r.closed {
		return r.closeErr
	}
	for id, h := range r.active {
		if !h.ended {
			_ = r.writeEndLocked(h, "incomplete", 0, "request ended when recorder closed", nil, Timings{}, EndDetails{})
			h.ended = true
		}
		delete(r.active, id)
	}
	if r.file != nil {
		if r.unsyncedRecords > 0 && r.failure == nil {
			if err := r.file.Sync(); err != nil {
				r.markSyncFailureLocked(err)
			}
		}
		if err := r.file.Close(); err != nil {
			if r.closeErr == nil {
				r.closeErr = fmt.Errorf("close request history file: %w", err)
			}
		}
		r.file = nil
	}
	r.closed = true
	if r.closeErr != nil {
		return r.closeErr
	}
	if r.failure != nil {
		r.closeErr = ErrRecordingUnavailable
		return r.closeErr
	}
	return nil
}

type baseLine struct {
	SchemaVersion int       `json:"schema_version"`
	Type          string    `json:"type"`
	RequestID     string    `json:"request_id"`
	StartedAt     time.Time `json:"started_at"`
	TimestampUTC  time.Time `json:"timestamp_utc"`
	ElapsedNS     int64     `json:"elapsed_ns"`
}

type startLine struct {
	baseLine
	ServiceAccount  ServiceAccountSnapshot `json:"service_account"`
	Model           ModelSnapshot          `json:"model"`
	IncomingRequest json.RawMessage        `json:"incoming_request"`
}

type transmitLine struct {
	baseLine
	Attempt          int             `json:"attempt"`
	ProviderID       string          `json:"provider_id"`
	ProviderName     string          `json:"provider_name"`
	UpstreamModel    string          `json:"upstream_model"`
	EffectiveRequest json.RawMessage `json:"effective_request"`
}

type eventLine struct {
	baseLine
	Attempt   int    `json:"attempt"`
	EventName string `json:"event_name"`
	Sequence  uint64 `json:"sequence"`
	WireText  string `json:"wire_text"`
}

type upstreamResponseLine struct {
	baseLine
	Attempt           int    `json:"attempt"`
	HTTPStatus        int    `json:"http_status"`
	ContentType       string `json:"content_type"`
	UpstreamRequestID string `json:"upstream_request_id"`
	Body              []byte `json:"body"`
	BodyTruncated     bool   `json:"body_truncated"`
	BodyReadFailed    bool   `json:"body_read_failed"`
}

type endLine struct {
	baseLine
	Attempt           int                    `json:"attempt"`
	Outcome           string                 `json:"outcome"`
	HTTPStatus        int                    `json:"http_status"`
	SafeError         string                 `json:"safe_error"`
	Usage             *Usage                 `json:"usage"`
	Timings           Timings                `json:"timings"`
	ServiceAccount    ServiceAccountSnapshot `json:"service_account"`
	Model             ModelSnapshot          `json:"model"`
	Provider          *ProviderSnapshot      `json:"provider,omitempty"`
	UpstreamRequestID string                 `json:"upstream_request_id,omitempty"`
	DroppedFields     []string               `json:"dropped_fields,omitempty"`
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestID {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '-' && char != '_' && char != '.' && char != ':' {
			return false
		}
	}
	return true
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded), nil
}

func validUpstreamRequestID(id string) bool {
	if len(id) > 256 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// validContentTypeHeader accepts an empty value, because a missing upstream
// Content-Type header is exactly what must stay visible in history.
func validContentTypeHeader(contentType string) bool {
	if len(contentType) > 512 {
		return false
	}
	for i := 0; i < len(contentType); i++ {
		if contentType[i] < 0x20 || contentType[i] > 0x7e {
			return false
		}
	}
	return true
}

func validOutcome(outcome string) bool {
	switch outcome {
	case "completed", "failed", "cancelled", "incomplete":
		return true
	default:
		return false
	}
}

func validUsage(usage *Usage) bool {
	if usage == nil {
		return true
	}
	for _, value := range []*int64{usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.ReasoningOutputTokens} {
		if value != nil && *value < 0 {
			return false
		}
	}
	return true
}

func cloneUsage(usage *Usage) *Usage {
	copyCount := func(source *int64) *int64 {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	return &Usage{
		InputTokens:           copyCount(usage.InputTokens),
		OutputTokens:          copyCount(usage.OutputTokens),
		CachedInputTokens:     copyCount(usage.CachedInputTokens),
		ReasoningOutputTokens: copyCount(usage.ReasoningOutputTokens),
	}
}

func validTimings(timings Timings) bool {
	for _, value := range []*int64{
		timings.AuthenticationPreparationNS,
		timings.ConnectionRequestedNS,
		timings.ConnectionEstablishedNS,
		timings.RequestTransmissionNS,
		timings.FirstEventNS,
		timings.FirstOutputNS,
		timings.StreamCompletionNS,
		timings.DownstreamDeliveryNS,
	} {
		if value != nil && *value < 0 {
			return false
		}
	}
	return true
}

func validJSONObject(raw []byte) bool {
	if !json.Valid(raw) {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	return object != nil
}

func cloneTimings(timings Timings) Timings {
	copyOffset := func(source *int64) *int64 {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	return Timings{
		AuthenticationPreparationNS: copyOffset(timings.AuthenticationPreparationNS),
		ConnectionRequestedNS:       copyOffset(timings.ConnectionRequestedNS),
		ConnectionEstablishedNS:     copyOffset(timings.ConnectionEstablishedNS),
		RequestTransmissionNS:       copyOffset(timings.RequestTransmissionNS),
		FirstEventNS:                copyOffset(timings.FirstEventNS),
		FirstOutputNS:               copyOffset(timings.FirstOutputNS),
		StreamCompletionNS:          copyOffset(timings.StreamCompletionNS),
		DownstreamDeliveryNS:        copyOffset(timings.DownstreamDeliveryNS),
		ConnectionReused:            cloneBool(timings.ConnectionReused),
	}
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
