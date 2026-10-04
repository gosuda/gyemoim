package history

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StorageStatus contains safe byte and compression metadata. It never includes
// record contents, credentials, or process stderr.
type StorageStatus struct {
	RecorderState               string         `json:"recorderState"`
	PotentiallyLostRecords      uint64         `json:"potentiallyLostRecords"`
	RecoveredBytes              uint64         `json:"recoveredBytes"`
	ActiveRequests              int            `json:"activeRequests"`
	ActiveBytes                 int64          `json:"activeBytes"`
	ActiveSegments              int            `json:"activeSegments"`
	RawSegments                 int            `json:"rawSegments"`
	RawBytes                    int64          `json:"rawBytes"`
	CompressedSegments          int            `json:"compressedSegments"`
	CompressedBytes             int64          `json:"compressedBytes"`
	PendingSegments             int            `json:"pendingSegments"`
	PendingBytes                int64          `json:"pendingBytes"`
	FailedSegments              int            `json:"failedSegments"`
	CompressionAvailable        bool           `json:"compressionAvailable"`
	CompressionState            string         `json:"compressionState"`
	LastCompressionError        string         `json:"lastCompressionError,omitempty"`
	LastCompressionSegment      string         `json:"lastCompressionSegment,omitempty"`
	LastCompressionAt           *time.Time     `json:"lastCompressionAt,omitempty"`
	CompressedValidationPending bool           `json:"compressedValidationPending"`
	Deletion                    DeletionStatus `json:"deletion"`
}

func listCompressedSegments(dir string) ([]ClosedSegment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	segments := make([]ClosedSegment, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, closedFilePrefix) || !strings.HasSuffix(name, closedFileSuffix+compressedSuffix) {
			continue
		}
		rawName := strings.TrimSuffix(name, compressedSuffix)
		if !validClosedFileName(rawName) {
			return nil, fmt.Errorf("unrecognized compressed history filename %q", name)
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("compressed history segment %q is not a regular file", name)
		}
		segments = append(segments, ClosedSegment{Name: name, Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Name < segments[j].Name })
	return segments, nil
}

func cleanupCompressionTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		marker := strings.LastIndex(name, closedFileSuffix+compressedSuffix+compressedTempMarker)
		if marker < 0 {
			continue
		}
		base := name[:marker+len(closedFileSuffix)]
		suffix := name[marker+len(closedFileSuffix+compressedSuffix+compressedTempMarker):]
		if !validClosedFileName(base) || !validRandomSuffix(suffix) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// A link or directory with a temp-looking name is not ours to follow or
		// remove. Only regular temporary output files are eligible for cleanup.
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(dir)
	}
	return nil
}

func validRandomSuffix(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && strings.ToLower(value) == value
}

func syncCompressedFile(path string) error {
	file, err := openRegularFile(path)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (r *Recorder) noteCompressionError(message, segment string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.compressionLastError = message
	r.compressionLastSegment = segment
	now := time.Now().UTC()
	r.compressionLastAt = now
	if segment != "" {
		r.compressionFailures[segment] = true
	}
	r.mu.Unlock()
}

// Storage returns current segment sizes and safe compression state. Filesystem
// scanning is metadata-only and occurs outside the recorder lock after snapshots
// have been copied.
func (r *Recorder) Storage() StorageStatus {
	if r == nil || r.historyDir == "" {
		return StorageStatus{CompressionState: "unavailable"}
	}
	r.mu.Lock()
	dir := r.historyDir
	activeBytes := r.bytesWritten
	activePresent := r.file != nil
	recorderState := r.state
	if r.closed {
		recorderState = "closed"
	}
	potentiallyLost := r.potentiallyLostRecords
	recoveredBytes := r.recoveredBytes
	activeRequests := len(r.active)
	executable := r.zstdExecutable
	lastError := r.compressionLastError
	lastSegment := r.compressionLastSegment
	lastAt := r.compressionLastAt
	validationPending := r.compressedQueryErr != nil || r.deletionCompressedValidationPending
	failures := make(map[string]bool, len(r.compressionFailures))
	for name, failed := range r.compressionFailures {
		failures[name] = failed
	}
	r.mu.Unlock()

	status := StorageStatus{RecorderState: recorderState, PotentiallyLostRecords: potentiallyLost, RecoveredBytes: recoveredBytes, ActiveRequests: activeRequests,
		ActiveBytes: activeBytes, CompressionAvailable: executable != "", LastCompressionError: lastError,
		LastCompressionSegment: lastSegment, CompressedValidationPending: validationPending, Deletion: r.DeletionStatus()}
	if activePresent {
		status.ActiveSegments = 1
	}
	if !lastAt.IsZero() {
		at := lastAt
		status.LastCompressionAt = &at
	}
	if status.CompressionAvailable {
		if err := ensureZstdExecutable(executable); err != nil {
			status.CompressionAvailable = false
		}
	}
	raw, rawErr := listClosedSegments(dir)
	compressed, compressedErr := listCompressedSegments(dir)
	if rawErr != nil || compressedErr != nil {
		status.CompressionState = "failed"
		if status.LastCompressionError == "" {
			status.LastCompressionError = "history segment metadata could not be read"
		}
		return status
	}
	compressedNames := make(map[string]bool, len(compressed))
	failedNames := make(map[string]bool)
	for _, segment := range compressed {
		compressedNames[segment.Name] = true
		status.CompressedSegments++
		status.CompressedBytes += segment.Size
		baseName := strings.TrimSuffix(segment.Name, compressedSuffix)
		if failures[segment.Name] || failures[baseName] {
			failedNames[baseName] = true
		}
	}
	for _, segment := range raw {
		status.RawSegments++
		status.RawBytes += segment.Size
		compressedName := segment.Name + compressedSuffix
		if !compressedNames[compressedName] {
			status.PendingSegments++
			status.PendingBytes += segment.Size
		}
		if failures[segment.Name] || failures[compressedName] {
			failedNames[segment.Name] = true
		}
	}
	status.FailedSegments = len(failedNames)
	if !status.CompressionAvailable {
		status.CompressionState = "unavailable"
	} else if status.FailedSegments > 0 || status.LastCompressionError != "" {
		status.CompressionState = "failed"
	} else if status.PendingSegments > 0 {
		status.CompressionState = "pending"
	} else {
		status.CompressionState = "idle"
	}
	return status
}

func (r *Recorder) compressPending(ctx context.Context) {
	if r == nil || ctx == nil || r.zstdExecutable == "" {
		return
	}
	r.retryVerifiedSourceCleanup(ctx)
	skipped := make(map[string]bool)
	for ctx.Err() == nil {
		segment, found, err := r.nextCompressionCandidate(skipped)
		if err != nil {
			r.noteCompressionError("history segment metadata could not be read", "")
			return
		}
		if !found {
			return
		}
		lease, err := r.BeginMaintenance(ctx)
		if err != nil {
			return
		}
		// Another operation may have published a matching representation while
		// this worker waited for existing readers to leave.
		current, stillPending, selectErr := r.compressionCandidateByName(segment.Name)
		if selectErr != nil || !stillPending {
			_ = lease.Close()
			if selectErr != nil {
				r.noteCompressionError("history segment metadata could not be read", segment.Name)
				return
			}
			continue
		}
		err = r.compressClosedSegment(ctx, current)
		_ = lease.Close()
		if err != nil {
			skipped[current.Name] = true
			r.noteCompressionError(compressionErrorLabel(err), current.Name)
			continue
		}
		r.mu.Lock()
		delete(r.compressionFailures, current.Name)
		r.compressionLastError = ""
		r.compressionLastSegment = current.Name
		now := time.Now().UTC()
		r.compressionLastAt = now
		r.mu.Unlock()
	}
}

func compressionErrorLabel(err error) string {
	if errors.Is(err, ErrZstdUnavailable) {
		return "zstd executable is unavailable"
	}
	return "a history segment could not be compressed or published"
}

func (r *Recorder) nextCompressionCandidate(skipped map[string]bool) (ClosedSegment, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.failure != nil || r.zstdExecutable == "" {
		return ClosedSegment{}, false, nil
	}
	segments, err := listClosedSegments(r.historyDir)
	if err != nil {
		return ClosedSegment{}, false, err
	}
	compressed, err := listCompressedSegments(r.historyDir)
	if err != nil {
		return ClosedSegment{}, false, err
	}
	existing := make(map[string]bool, len(compressed))
	for _, segment := range compressed {
		existing[segment.Name] = true
	}
	for _, segment := range segments {
		if skipped[segment.Name] || existing[segment.Name+compressedSuffix] || r.verifiedCompressed[segment.Name] {
			continue
		}
		return segment, true, nil
	}
	return ClosedSegment{}, false, nil
}

func (r *Recorder) compressionCandidateByName(name string) (ClosedSegment, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	segments, err := listClosedSegments(r.historyDir)
	if err != nil {
		return ClosedSegment{}, false, err
	}
	compressed, err := listCompressedSegments(r.historyDir)
	if err != nil {
		return ClosedSegment{}, false, err
	}
	for _, segment := range segments {
		if segment.Name != name {
			continue
		}
		for _, output := range compressed {
			if output.Name == segment.Name+compressedSuffix {
				return ClosedSegment{}, false, nil
			}
		}
		if r.verifiedCompressed[segment.Name] {
			return ClosedSegment{}, false, nil
		}
		return segment, true, nil
	}
	return ClosedSegment{}, false, nil
}

func (r *Recorder) retryVerifiedSourceCleanup(ctx context.Context) {
	r.mu.Lock()
	names := make([]string, 0, len(r.verifiedCompressed))
	for name, verified := range r.verifiedCompressed {
		if verified {
			names = append(names, name)
		}
	}
	r.mu.Unlock()
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		sourcePath := filepath.Join(r.historyDir, name)
		if _, err := os.Lstat(sourcePath); errors.Is(err, os.ErrNotExist) {
			lease, leaseErr := r.BeginMaintenance(ctx)
			if leaseErr != nil {
				return
			}
			compressedName := name + compressedSuffix
			compressedPath := filepath.Join(r.historyDir, compressedName)
			syncErr := syncCompressedFile(compressedPath)
			if syncErr == nil {
				syncErr = syncDirectory(r.historyDir)
			}
			if syncErr == nil {
				r.mu.Lock()
				delete(r.verifiedCompressed, name)
				delete(r.compressionFailures, name)
				delete(r.compressionFailures, compressedName)
				if r.compressionLastSegment == name || r.compressionLastSegment == compressedName {
					r.compressionLastError = ""
				}
				r.mu.Unlock()
			} else {
				r.noteCompressionError("verified compressed copy could not be synced", name)
			}
			_ = lease.Close()
			continue
		} else if err != nil {
			r.noteCompressionError("verified raw source could not be inspected", name)
			continue
		}
		lease, err := r.BeginMaintenance(ctx)
		if err != nil {
			return
		}
		compressedName := name + compressedSuffix
		compressedPath := filepath.Join(r.historyDir, compressedName)
		compressedDigest, compressedLength, verifyErr := validateCompressedHistory(ctx, r.zstdExecutable, compressedPath, nil)
		if verifyErr == nil {
			var rawDigest [32]byte
			var rawLength int64
			rawDigest, rawLength, verifyErr = hashRegularFile(ctx, sourcePath)
			if verifyErr == nil && (compressedLength != rawLength || compressedDigest != rawDigest) {
				verifyErr = errors.New("raw and compressed history copies do not match")
			}
		}
		if verifyErr != nil {
			r.mu.Lock()
			r.compressedQueryErr = fmt.Errorf("%w: pair %q failed verification", ErrCompressedHistoryInvalid, name)
			r.mu.Unlock()
			r.noteCompressionError("raw and compressed history copies do not match", name)
			_ = lease.Close()
			continue
		}
		if err := syncCompressedFile(compressedPath); err != nil {
			r.noteCompressionError("verified compressed copy could not be synced", name)
			_ = lease.Close()
			continue
		}
		if err := syncDirectory(r.historyDir); err != nil {
			r.noteCompressionError("history directory could not be synced before source removal", name)
			_ = lease.Close()
			continue
		}
		if err := os.Remove(sourcePath); err != nil {
			r.noteCompressionError("verified raw source could not be removed", name)
			_ = lease.Close()
			continue
		}
		if err := syncDirectory(r.historyDir); err != nil {
			r.noteCompressionError("history directory could not be synced after source removal", name)
		} else {
			r.mu.Lock()
			delete(r.verifiedCompressed, name)
			delete(r.compressionFailures, name)
			delete(r.compressionFailures, compressedName)
			r.compressionLastError = ""
			r.compressionLastSegment = name
			now := time.Now().UTC()
			r.compressionLastAt = now
			r.mu.Unlock()
		}
		_ = lease.Close()
	}
}

func (r *Recorder) compressClosedSegment(ctx context.Context, segment ClosedSegment) error {
	sourcePath := filepath.Join(r.historyDir, segment.Name)
	finalName := segment.Name + compressedSuffix
	finalPath := filepath.Join(r.historyDir, finalName)
	source, err := openRegularFile(sourcePath)
	if err != nil {
		return fmt.Errorf("open raw source: %w", err)
	}
	defer source.Close()
	if _, err := os.Lstat(finalPath); err == nil {
		return errors.New("compressed destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect compressed destination: %w", err)
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("create temporary filename: %w", err)
	}
	tempName := finalName + compressedTempMarker + hex.EncodeToString(suffix[:])
	tempPath := filepath.Join(r.historyDir, tempName)
	temp, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary compressed output: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		createdInfo, _ := temp.Stat()
		_ = temp.Close()
		if createdInfo != nil {
			if current, statErr := os.Lstat(tempPath); statErr == nil && current.Mode().IsRegular() && os.SameFile(createdInfo, current) {
				_ = os.Remove(tempPath)
				_ = syncDirectory(r.historyDir)
			}
		}
		return fmt.Errorf("set temporary output permissions: %w", err)
	}
	tempInfo, err := temp.Stat()
	if err != nil {
		_ = temp.Close()
		return fmt.Errorf("inspect temporary compressed output: %w", err)
	}
	if !tempInfo.Mode().IsRegular() {
		_ = temp.Close()
		return errors.New("temporary compressed output is not a regular file")
	}
	published := false
	defer func() {
		if !published {
			_ = temp.Close()
			if tempInfo != nil {
				if current, statErr := os.Lstat(tempPath); statErr == nil && current.Mode().IsRegular() && os.SameFile(tempInfo, current) {
					if os.Remove(tempPath) == nil {
						_ = syncDirectory(r.historyDir)
					}
				}
			}
		}
	}()
	if err := runZstdCompression(ctx, r.zstdExecutable, source, temp); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary compressed output: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary compressed output: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("publish compressed output: %w", err)
	}
	published = true
	r.mu.Lock()
	r.verifiedCompressed[segment.Name] = true
	r.mu.Unlock()
	if err := syncDirectory(r.historyDir); err != nil {
		return fmt.Errorf("sync history directory after compressed publication: %w", err)
	}
	if err := os.Remove(sourcePath); err != nil {
		return fmt.Errorf("remove raw source after publication: %w", err)
	}
	if err := syncDirectory(r.historyDir); err != nil {
		return fmt.Errorf("sync history directory after raw source removal: %w", err)
	}
	r.mu.Lock()
	delete(r.verifiedCompressed, segment.Name)
	r.mu.Unlock()
	return nil
}
