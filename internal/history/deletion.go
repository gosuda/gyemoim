package history

import (
	"bufio"
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
	"time"
)

const (
	deletionJournalName = ".history-delete.json"
	deletionTempPrefix  = ".history-delete-tmp-"
	deletionTimeout     = 15 * time.Minute
)

var (
	ErrInvalidDeletionRange = errors.New("invalid history deletion date range")
	ErrActiveRequestOverlap = errors.New("an active request overlaps the history deletion range")
	ErrDeletionPending      = errors.New("history deletion is pending recovery")
)

type deletionEntry struct {
	SourceName     string `json:"sourceName"`
	RawName        string `json:"rawName"`
	Compressed     bool   `json:"compressed"`
	Completed      bool   `json:"completed"`
	RecordsRemoved uint64 `json:"recordsRemoved"`
}

type deletionJournal struct {
	Version int             `json:"version"`
	From    time.Time       `json:"from"`
	To      time.Time       `json:"to"`
	Entries []deletionEntry `json:"entries"`
}

// DeletionStatus contains safe progress and failure information for the last or
// currently pending date-range maintenance operation.
type DeletionStatus struct {
	State             string     `json:"state"`
	From              *time.Time `json:"from,omitempty"`
	To                *time.Time `json:"to,omitempty"`
	CompletedSegments int        `json:"completedSegments"`
	TotalSegments     int        `json:"totalSegments"`
	Error             string     `json:"error,omitempty"`
	UpdatedAt         *time.Time `json:"updatedAt,omitempty"`
}

// DeleteResult describes the effective UTC half-open interval and its completion.
type DeleteResult struct {
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	SegmentsProcessed int       `json:"segmentsProcessed"`
	RecordsRemoved    uint64    `json:"recordsRemoved"`
}

func parseDeletionDate(value string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return time.Time{}, ErrInvalidDeletionRange
	}
	return parsed.UTC(), nil
}

func buildDeletionEntries(raw, compressed []ClosedSegment) ([]deletionEntry, error) {
	rawByName := make(map[string]bool, len(raw))
	for _, segment := range raw {
		if !validClosedFileName(segment.Name) {
			return nil, fmt.Errorf("invalid raw history filename %q", segment.Name)
		}
		rawByName[segment.Name] = true
	}
	compressedByName := make(map[string]bool, len(compressed))
	for _, segment := range compressed {
		rawName := strings.TrimSuffix(segment.Name, compressedSuffix)
		if !validClosedFileName(rawName) || segment.Name != rawName+compressedSuffix {
			return nil, fmt.Errorf("invalid compressed history filename %q", segment.Name)
		}
		compressedByName[rawName] = true
	}
	bases := make(map[string]bool, len(rawByName)+len(compressedByName))
	for name := range rawByName {
		bases[name] = true
	}
	for name := range compressedByName {
		bases[name] = true
	}
	entries := make([]deletionEntry, 0, len(bases))
	for rawName := range bases {
		compressedSource := compressedByName[rawName]
		sourceName := rawName
		if compressedSource {
			sourceName += compressedSuffix
		} else if !rawByName[rawName] {
			return nil, fmt.Errorf("history representation for %q is missing", rawName)
		}
		entries = append(entries, deletionEntry{SourceName: sourceName, RawName: rawName, Compressed: compressedSource})
	}
	sortDeletionEntries(entries)
	return entries, nil
}

func sortDeletionEntries(entries []deletionEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].RawName < entries[j].RawName })
}

const maxDeletionJournalBytes = 16 << 20

func checkDeletionJournalSize(journal deletionJournal) error {
	worstCase := journal
	worstCase.Entries = append([]deletionEntry(nil), journal.Entries...)
	for index := range worstCase.Entries {
		// Every progress checkpoint must fit even after each counter reaches the
		// maximum uint64 decimal width. `false` is also the longer JSON boolean.
		worstCase.Entries[index].RecordsRemoved = ^uint64(0)
		worstCase.Entries[index].Completed = false
	}
	encoded, err := json.Marshal(worstCase)
	if err != nil {
		return err
	}
	if len(encoded) > maxDeletionJournalBytes-512 {
		return errors.New("history deletion spans too many segments for a safe journal")
	}
	return nil
}

func validateDeletionJournal(j deletionJournal) error {
	if j.Version != 1 || j.From.IsZero() || j.To.IsZero() || !j.From.Before(j.To) || len(j.Entries) == 0 {
		return errors.New("deletion journal has invalid range or version")
	}
	if _, offset := j.From.Zone(); offset != 0 {
		return errors.New("deletion journal start is not UTC")
	}
	if _, offset := j.To.Zone(); offset != 0 {
		return errors.New("deletion journal end is not UTC")
	}
	seen := make(map[string]bool, len(j.Entries))
	for _, entry := range j.Entries {
		if !validClosedFileName(entry.RawName) || seen[entry.RawName] {
			return errors.New("deletion journal contains an invalid or duplicate segment")
		}
		seen[entry.RawName] = true
		if entry.Compressed {
			if entry.SourceName != entry.RawName+compressedSuffix {
				return errors.New("deletion journal compressed filename does not match its segment")
			}
		} else if entry.SourceName != entry.RawName {
			return errors.New("deletion journal raw filename does not match its segment")
		}
	}
	return nil
}

func readDeletionJournal(dir string) (deletionJournal, error) {
	path := filepath.Join(dir, deletionJournalName)
	file, err := openRegularFile(path)
	if err != nil {
		return deletionJournal{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return deletionJournal{}, err
	}
	if info.Size() > 16<<20 {
		return deletionJournal{}, errors.New("deletion journal is too large")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return deletionJournal{}, errors.New("deletion journal permissions are not owner-only")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	var journal deletionJournal
	if err := decoder.Decode(&journal); err != nil {
		return deletionJournal{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return deletionJournal{}, errors.New("deletion journal has trailing content")
	}
	if err := validateDeletionJournal(journal); err != nil {
		return deletionJournal{}, err
	}
	return journal, nil
}

func uniqueDeletionTempName() (string, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", err
	}
	return deletionTempPrefix + hex.EncodeToString(random[:]), nil
}

func publishDeletionJournal(dir string, journal deletionJournal, requireAbsent bool) (bool, error) {
	if err := validateDeletionJournal(journal); err != nil {
		return false, err
	}
	if err := checkDeletionJournalSize(journal); err != nil {
		return false, err
	}
	journalPath := filepath.Join(dir, deletionJournalName)
	if requireAbsent {
		if _, err := os.Lstat(journalPath); err == nil {
			return false, errors.New("a history deletion journal already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	} else {
		info, err := os.Lstat(journalPath)
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, errors.New("history deletion journal is not a regular file")
		}
	}
	tempName, err := uniqueDeletionTempName()
	if err != nil {
		return false, err
	}
	tempPath := filepath.Join(dir, tempName)
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	fileInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return false, statErr
	}
	published := false
	defer func() {
		if !published {
			_ = file.Close()
			if current, err := os.Lstat(tempPath); err == nil && current.Mode().IsRegular() && os.SameFile(fileInfo, current) {
				_ = os.Remove(tempPath)
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		return false, err
	}
	encoded = append(encoded, '\n')
	if err := writeAll(file, encoded); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, journalPath); err != nil {
		return false, err
	}
	published = true
	if err := syncDirectory(dir); err != nil {
		return true, err
	}
	return true, nil
}

func cleanDeletionTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, deletionTempPrefix) || !validRandomSuffix(strings.TrimPrefix(name, deletionTempPrefix)) {
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
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
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

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("history writer returned an invalid byte count")
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func rewriteHistoryRecords(ctx context.Context, source io.Reader, output io.Writer, from, to time.Time) (uint64, error) {
	reader := bufio.NewReaderSize(source, recordReadBufferSize)
	var removed uint64
	for {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		line, terminated, err := readBoundedLine(ctx, reader)
		if err == io.EOF {
			return removed, nil
		}
		if err != nil {
			return removed, err
		}
		if !terminated {
			return removed, errors.New("history segment has an incomplete final record")
		}
		record, err := decodeRecord(line)
		if err != nil {
			return removed, err
		}
		if !record.StartedAt.Before(from) && record.StartedAt.Before(to) {
			removed++
			continue
		}
		if err := writeAll(output, line); err != nil {
			return removed, err
		}
		if err := writeAll(output, []byte{'\n'}); err != nil {
			return removed, err
		}
	}
}

func (r *Recorder) rewriteDeletionEntry(ctx context.Context, entry *deletionEntry, journal *deletionJournal) (uint64, error) {
	sourcePath := filepath.Join(r.historyDir, entry.SourceName)
	input, err := openRegularFile(sourcePath)
	if err != nil {
		return 0, fmt.Errorf("open deletion source: %w", err)
	}
	defer input.Close()
	tempName, err := uniqueDeletionTempName()
	if err != nil {
		return 0, err
	}
	tempPath := filepath.Join(r.historyDir, tempName)
	temp, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	tempInfo, err := temp.Stat()
	if err != nil {
		_ = temp.Close()
		return 0, err
	}
	published := false
	defer func() {
		if !published {
			_ = temp.Close()
			if current, err := os.Lstat(tempPath); err == nil && current.Mode().IsRegular() && os.SameFile(tempInfo, current) {
				if os.Remove(tempPath) == nil {
					_ = syncDirectory(r.historyDir)
				}
			}
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return 0, err
	}
	var source io.Reader = input
	var process *zstdReader
	if entry.Compressed {
		process, err = startZstdReader(ctx, r.zstdExecutable, input)
		if err != nil {
			return 0, err
		}
		source = process
	}
	removed, rewriteErr := rewriteHistoryRecords(ctx, source, temp, journal.From, journal.To)
	if process != nil {
		rewriteErr = process.Finish(rewriteErr)
	}
	if rewriteErr != nil {
		return removed, rewriteErr
	}
	if err := temp.Sync(); err != nil {
		return removed, err
	}
	if err := temp.Close(); err != nil {
		return removed, err
	}
	if err := ctx.Err(); err != nil {
		return removed, err
	}
	if removed > entry.RecordsRemoved {
		entry.RecordsRemoved = removed
	}
	if _, err := publishDeletionJournal(r.historyDir, *journal, false); err != nil {
		return removed, err
	}
	rawPath := filepath.Join(r.historyDir, entry.RawName)
	// For raw sources this atomically replaces the source. For compressed sources
	// this publishes the filtered raw representation before the journal lets us
	// unlink the .zst file.
	if err := os.Rename(tempPath, rawPath); err != nil {
		return removed, err
	}
	published = true
	if err := syncDirectory(r.historyDir); err != nil {
		return removed, err
	}
	return removed, nil
}

func removeDeletionSource(dir string, name string) error {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("deletion source is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (r *Recorder) applyDeletionJournal(ctx context.Context, journal *deletionJournal) (DeleteResult, error) {
	result := DeleteResult{From: journal.From, To: journal.To, SegmentsProcessed: len(journal.Entries)}
	for index := range journal.Entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entry := &journal.Entries[index]
		if !entry.Completed {
			_, err := r.rewriteDeletionEntry(ctx, entry, journal)
			if err != nil {
				return result, err
			}
			entry.Completed = true
			if _, err := publishDeletionJournal(r.historyDir, *journal, false); err != nil {
				return result, err
			}
		}
		output, outputErr := openRegularFile(filepath.Join(r.historyDir, entry.RawName))
		if outputErr != nil {
			return result, fmt.Errorf("completed deletion output is unavailable: %w", outputErr)
		}
		_ = output.Close()
		if entry.Compressed {
			// The raw replacement is now the sole published representation. The
			// compressed source can be removed only after its done checkpoint.
			if err := removeDeletionSource(r.historyDir, entry.SourceName); err != nil {
				return result, err
			}
		}
		result.RecordsRemoved += entry.RecordsRemoved
		r.updateDeletionProgress(index + 1)
	}
	if err := os.Remove(filepath.Join(r.historyDir, deletionJournalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := syncDirectory(r.historyDir); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Recorder) updateDeletionProgress(completed int) {
	r.mu.Lock()
	r.deletionCompletedSegments = completed
	r.deletionUpdatedAt = time.Now().UTC()
	r.mu.Unlock()
}

func (r *Recorder) markDeletionPending(journal *deletionJournal, lease *MaintenanceLease, err error) {
	r.mu.Lock()
	r.deletionState = "pending_recovery"
	r.deletionError = "history deletion could not finish; history queries remain unavailable until recovery succeeds"
	if journal != nil {
		r.pendingDeletionEntries = make(map[string]bool, len(journal.Entries))
		from, to := journal.From, journal.To
		r.deletionFrom = &from
		r.deletionTo = &to
		r.deletionTotalSegments = len(journal.Entries)
		completed := 0
		for _, entry := range journal.Entries {
			r.pendingDeletionEntries[entry.RawName] = true
			if entry.Compressed {
				r.deletionCompressedValidationPending = true
			}
			if entry.Completed {
				completed++
			}
		}
		r.deletionCompletedSegments = completed
	}
	r.deletionUpdatedAt = time.Now().UTC()
	r.pendingMaintenance = lease
	r.mu.Unlock()
	_ = err // Internal diagnostics are intentionally not exposed in status or API.
}

func (r *Recorder) finishDeletion(lease *MaintenanceLease, journal *deletionJournal, result DeleteResult) {
	r.mu.Lock()
	from, to := result.From, result.To
	r.deletionState = "completed"
	r.deletionFrom = &from
	r.deletionTo = &to
	r.deletionCompletedSegments = result.SegmentsProcessed
	r.deletionTotalSegments = result.SegmentsProcessed
	r.deletionError = ""
	r.deletionUpdatedAt = time.Now().UTC()
	r.pendingMaintenance = nil
	r.pendingDeletionEntries = nil
	r.deletionCompressedValidationPending = false
	for _, entry := range journal.Entries {
		delete(r.verifiedCompressed, entry.RawName)
		delete(r.compressionFailures, entry.RawName)
		delete(r.compressionFailures, entry.SourceName)
		if r.compressionLastSegment == entry.RawName || r.compressionLastSegment == entry.SourceName {
			r.compressionLastError = ""
		}
	}
	r.mu.Unlock()
	_ = lease.Close()
}

// validateUntouchedHistory keeps ordinary closed-file checks for segments outside
// a pending deletion journal. Journal-owned files may intentionally have mismatched
// raw/.zst representations while a replacement is being replayed.
func (r *Recorder) validateUntouchedHistory() error {
	raw, err := listClosedSegments(r.historyDir)
	if err != nil {
		if r.pendingDeletionEntries == nil {
			return nil
		}
		return fmt.Errorf("list unrelated history segments: %w", err)
	}
	compressed, err := listCompressedSegments(r.historyDir)
	if err != nil {
		if r.pendingDeletionEntries == nil {
			return nil
		}
		return fmt.Errorf("list unrelated compressed history segments: %w", err)
	}
	skip := func(name string) bool {
		return r.pendingDeletionEntries == nil || r.pendingDeletionEntries[name]
	}
	for _, segment := range raw {
		if skip(segment.Name) {
			continue
		}
		if err := validateHistoryFile(filepath.Join(r.historyDir, segment.Name), false, nil); err != nil {
			return fmt.Errorf("unrelated closed history segment %q is invalid: %w", segment.Name, err)
		}
	}
	if len(compressed) > 0 && r.zstdExecutable == "" {
		r.compressedQueryErr = ErrZstdUnavailable
		r.noteCompressionError("unrelated compressed history cannot be validated because zstd is unavailable", "")
		return nil
	}
	for _, segment := range compressed {
		rawName := strings.TrimSuffix(segment.Name, compressedSuffix)
		if skip(rawName) {
			r.deletionCompressedValidationPending = true
			continue
		}
		compressedPath := filepath.Join(r.historyDir, segment.Name)
		digest, length, validationErr := validateCompressedHistory(context.Background(), r.zstdExecutable, compressedPath, nil)
		if validationErr != nil {
			r.compressedQueryErr = fmt.Errorf("%w: segment %q could not be validated", ErrCompressedHistoryInvalid, segment.Name)
			r.noteCompressionError("unrelated compressed history failed startup validation", segment.Name)
			return fmt.Errorf("unrelated compressed history segment %q is invalid: %w", segment.Name, validationErr)
		}
		if err := syncCompressedFile(compressedPath); err != nil {
			r.compressedQueryErr = fmt.Errorf("%w: segment %q could not be synced", ErrCompressedHistoryInvalid, segment.Name)
			return fmt.Errorf("sync unrelated compressed history segment %q: %w", segment.Name, err)
		}
		rawPath := filepath.Join(r.historyDir, rawName)
		if _, statErr := os.Lstat(rawPath); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			r.compressedQueryErr = fmt.Errorf("%w: pair %q could not be inspected", ErrCompressedHistoryInvalid, rawName)
			return fmt.Errorf("inspect unrelated raw source for %q: %w", segment.Name, statErr)
		}
		rawDigest, rawLength, hashErr := hashRegularFile(context.Background(), rawPath)
		if hashErr != nil || rawLength != length || rawDigest != digest {
			r.compressedQueryErr = fmt.Errorf("%w: pair %q does not match", ErrCompressedHistoryInvalid, rawName)
			r.noteCompressionError("unrelated raw and compressed history copies do not match", rawName)
			if hashErr != nil {
				return fmt.Errorf("compare unrelated raw and compressed history segment %q: %w", rawName, hashErr)
			}
			return fmt.Errorf("unrelated raw and compressed history segment pair %q does not match", rawName)
		}
		r.verifiedCompressed[rawName] = true
		if err := syncDirectory(r.historyDir); err != nil {
			r.compressionFailures[segment.Name] = true
			return fmt.Errorf("sync history directory before unrelated source removal: %w", err)
		}
		if err := os.Remove(rawPath); err != nil {
			r.compressionFailures[segment.Name] = true
			continue
		}
		if err := syncDirectory(r.historyDir); err != nil {
			r.compressionFailures[segment.Name] = true
		} else {
			delete(r.verifiedCompressed, rawName)
		}
	}
	return nil
}

// DeletionStatus returns a copy of the visible maintenance state.
func (r *Recorder) DeletionStatus() DeletionStatus {
	if r == nil {
		return DeletionStatus{State: "unavailable"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	status := DeletionStatus{State: r.deletionState, CompletedSegments: r.deletionCompletedSegments, TotalSegments: r.deletionTotalSegments, Error: r.deletionError}
	if r.deletionFrom != nil {
		from := *r.deletionFrom
		status.From = &from
	}
	if r.deletionTo != nil {
		to := *r.deletionTo
		status.To = &to
	}
	if !r.deletionUpdatedAt.IsZero() {
		updated := r.deletionUpdatedAt
		status.UpdatedAt = &updated
	}
	return status
}

func (r *Recorder) recoverDeletionAtStartup() bool {
	journalPath := filepath.Join(r.historyDir, deletionJournalName)
	if _, err := os.Lstat(journalPath); errors.Is(err, os.ErrNotExist) {
		_ = cleanDeletionTemps(r.historyDir)
		return false
	} else if err != nil {
		lease, leaseErr := r.BeginMaintenance(context.Background())
		if leaseErr == nil {
			r.markDeletionPending(nil, lease, err)
		}
		return true
	}
	lease, err := r.BeginMaintenance(context.Background())
	if err != nil {
		r.markDeletionPending(nil, nil, err)
		return true
	}
	journal, err := readDeletionJournal(r.historyDir)
	if err != nil {
		r.markDeletionPending(nil, lease, err)
		return true
	}
	r.deletionState = "running"
	r.deletionFrom = &journal.From
	r.deletionTo = &journal.To
	r.deletionTotalSegments = len(journal.Entries)
	ctx, cancel := context.WithTimeout(context.Background(), deletionTimeout)
	defer cancel()
	result, err := r.applyDeletionJournal(ctx, &journal)
	if err != nil {
		r.markDeletionPending(&journal, lease, err)
		return true
	}
	_ = cleanDeletionTemps(r.historyDir)
	r.finishDeletion(lease, &journal, result)
	return false
}

// DeleteDateRange removes every history record whose request started_at falls on
// an inclusive UTC calendar date. Requests outside the effective half-open range
// retain all of their records, including records written after the snapshot.
func (r *Recorder) DeleteDateRange(ctx context.Context, firstDate, lastDate string) (DeleteResult, error) {
	return r.DeleteDateRangeAt(ctx, firstDate, lastDate, time.Now().UTC())
}

// deletionInterval validates an inclusive UTC date range exactly as the
// date-range deletion does and returns its effective half-open interval
// [from, to), capped at the submission time. The read-only delete preview
// reuses this helper so the preview and the deletion can never drift apart.
func deletionInterval(firstDate, lastDate string, submittedAt time.Time) (time.Time, time.Time, error) {
	if submittedAt.IsZero() {
		return time.Time{}, time.Time{}, ErrInvalidDeletionRange
	}
	cutoff := submittedAt.UTC()
	from, err := parseDeletionDate(firstDate)
	if err != nil {
		return time.Time{}, time.Time{}, ErrInvalidDeletionRange
	}
	last, err := parseDeletionDate(lastDate)
	if err != nil || last.Before(from) {
		return time.Time{}, time.Time{}, ErrInvalidDeletionRange
	}
	today, _ := parseDeletionDate(cutoff.Format("2006-01-02"))
	if last.After(today) {
		return time.Time{}, time.Time{}, ErrInvalidDeletionRange
	}
	requestedTo := last.AddDate(0, 0, 1)
	to := requestedTo
	if to.After(cutoff) {
		to = cutoff
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, ErrInvalidDeletionRange
	}
	return from, to, nil
}

// DeleteDateRangeAt keeps the submitted cutoff stable while existing query leases
// drain. Recorder.Begin captures admission timestamps under the same writer mutex.
func (r *Recorder) DeleteDateRangeAt(ctx context.Context, firstDate, lastDate string, submittedAt time.Time) (DeleteResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.historyDir == "" || r.queryGate == nil {
		return DeleteResult{}, ErrQueryUnavailable
	}
	from, to, err := deletionInterval(firstDate, lastDate, submittedAt)
	if err != nil {
		return DeleteResult{}, err
	}
	lease, err := r.BeginMaintenance(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		_ = lease.Close()
		return DeleteResult{}, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			_ = lease.Close()
		}
	}()

	r.mu.Lock()
	if r.closed || r.closing || r.failure != nil || r.file == nil {
		r.mu.Unlock()
		return DeleteResult{}, ErrRecordingUnavailable
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	for _, request := range r.active {
		if request != nil && !request.startedAt.Before(from) && request.startedAt.Before(to) {
			r.mu.Unlock()
			return DeleteResult{}, ErrActiveRequestOverlap
		}
	}
	raw, rawErr := listClosedSegments(r.historyDir)
	compressed, compressedErr := listCompressedSegments(r.historyDir)
	if rawErr != nil || compressedErr != nil {
		r.mu.Unlock()
		return DeleteResult{}, fmt.Errorf("history segments could not be inspected")
	}
	if len(compressed) > 0 && ensureZstdExecutable(r.zstdExecutable) != nil {
		r.mu.Unlock()
		return DeleteResult{}, ErrZstdUnavailable
	}
	initialEntries, err := buildDeletionEntries(raw, compressed)
	if err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	projectedEntries := append([]deletionEntry(nil), initialEntries...)
	if r.bytesWritten > 0 {
		projectedEntries = append(projectedEntries, deletionEntry{SourceName: "segment-20000101T000000.000000000Z-00000000000000000000000000000000.ndjson", RawName: "segment-20000101T000000.000000000Z-00000000000000000000000000000000.ndjson"})
	}
	if err := checkDeletionJournalSize(deletionJournal{Version: 1, From: from, To: to, Entries: projectedEntries}); err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	if r.bytesWritten > 0 {
		if err := r.rotateLocked(submittedAt.UTC()); err != nil {
			r.mu.Unlock()
			return DeleteResult{}, err
		}
	}
	raw, rawErr = listClosedSegments(r.historyDir)
	compressed, compressedErr = listCompressedSegments(r.historyDir)
	if rawErr != nil || compressedErr != nil {
		r.mu.Unlock()
		return DeleteResult{}, errors.New("history segments could not be captured")
	}
	entries, err := buildDeletionEntries(raw, compressed)
	if err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	if len(entries) == 0 {
		r.mu.Unlock()
		return DeleteResult{From: from, To: to}, nil
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	journal := deletionJournal{Version: 1, From: from, To: to, Entries: entries}
	if err := checkDeletionJournalSize(journal); err != nil {
		r.mu.Unlock()
		return DeleteResult{}, err
	}
	published, publishErr := publishDeletionJournal(r.historyDir, journal, true)
	if publishErr != nil {
		if published {
			r.deletionState = "pending_recovery"
			r.deletionError = "history deletion intent could not be durably published; queries remain unavailable until recovery"
			r.deletionFrom = &from
			r.deletionTo = &to
			r.deletionTotalSegments = len(entries)
			r.deletionCompletedSegments = 0
			r.deletionUpdatedAt = time.Now().UTC()
			r.pendingMaintenance = lease
			keepLease = true
		}
		r.mu.Unlock()
		if published {
			return DeleteResult{}, fmt.Errorf("%w: %v", ErrDeletionPending, publishErr)
		}
		return DeleteResult{}, publishErr
	}
	r.deletionState = "running"
	r.deletionError = ""
	r.deletionFrom = &from
	r.deletionTo = &to
	r.deletionTotalSegments = len(entries)
	r.deletionCompletedSegments = 0
	r.deletionUpdatedAt = time.Now().UTC()
	serviceCtx, cancel := context.WithTimeout(context.Background(), deletionTimeout)
	serviceDone := make(chan struct{})
	r.deletionServiceCancel = cancel
	r.deletionServiceDone = serviceDone
	r.mu.Unlock()

	// Once the journal is durable, request cancellation cannot abandon the
	// roll-forward operation. The bounded service context either completes it or
	// leaves the journal and maintenance lease in place for startup replay.
	defer func() {
		cancel()
		r.mu.Lock()
		if r.deletionServiceDone == serviceDone {
			r.deletionServiceCancel = nil
			r.deletionServiceDone = nil
		}
		r.mu.Unlock()
		close(serviceDone)
	}()
	result, err := r.applyDeletionJournal(serviceCtx, &journal)
	if err != nil {
		r.markDeletionPending(&journal, lease, err)
		keepLease = true
		return result, fmt.Errorf("%w: %v", ErrDeletionPending, err)
	}
	cleanupErr := cleanDeletionTemps(r.historyDir)
	r.finishDeletion(lease, &journal, result)
	if cleanupErr != nil {
		r.mu.Lock()
		r.deletionError = "unused history deletion temporary files could not be cleaned up"
		r.deletionUpdatedAt = time.Now().UTC()
		r.mu.Unlock()
	}
	return result, nil
}
