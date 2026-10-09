package history

import (
	"context"
	"errors"
	"time"
)

// MaxDeletePreviewRecords bounds the delete-preview counting scan. Beyond it
// the preview reports capped=true instead of scanning the whole archive, so a
// preview over a huge history stays bounded in time and memory like every
// other history read.
const MaxDeletePreviewRecords = 100000

// previewSkipMargin compensates for filesystem timestamp granularity when a
// segment is skipped by modification time. A record's started_at never exceeds
// its write time, and a closed segment's last write never exceeds its
// modification time, so mtime is already a sound upper bound; the margin only
// guards against coarse mtime clocks rounding down. Two seconds cost nothing
// against segment lifetimes of minutes and hours.
const previewSkipMargin = 2 * time.Second

// DeletePreview is the read-only answer to "how much would the date-range
// deletion remove?". Records counts history record lines whose started_at
// falls inside the effective half-open deletion interval — the exact set
// rewriteHistoryRecords removes — across closed raw segments, closed
// compressed segments, and the active file through its captured length (the
// deletion rotates the active file first, so its records are removed too).
//
// Segments mirrors DeleteResult.SegmentsProcessed: the deletion rewrites every
// closed segment (plus the rotated active file), not only the ones holding
// matches, so the count is the number of files the operation would open and
// rewrite. Bytes is always null today: the removed bytes are interleaved with
// retained bytes inside immutable segment files, and neither the segment
// metadata nor the shared bounded record reader exposes per-line sizes, so no
// honest cheap number exists to report.
type DeletePreview struct {
	Records  uint64 `json:"records"`
	Segments int    `json:"segments"`
	Capped   bool   `json:"capped"`
	Bytes    *int64 `json:"bytes"`
}

// errPreviewCapped stops the counting scan once the cap is reached; the
// walk/scan layers wrap visitor errors, so the sentinel stays identifiable.
var errPreviewCapped = errors.New("history delete preview reached its record cap")

// DeletePreview counts what DeleteDateRange would remove for the same
// inclusive UTC dates without acquiring the maintenance lease or changing any
// history file. It takes a regular query lease, so it returns the same errors
// as every other bounded history read: ErrMaintenance (HTTP 503) while
// deletion, recovery, or compression holds exclusive maintenance, ErrQueryBusy
// when four reads are already active, and the context deadline produces the
// explicit 30-second timeout rather than a partial result.
func (q *QueryService) DeletePreview(ctx context.Context, firstDate, lastDate string) (DeletePreview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if q == nil || q.recorder == nil {
		return DeletePreview{}, ErrQueryUnavailable
	}
	// Same interval computation, source set, and started_at overlap semantics
	// as the deletion, by construction (shared helper with DeleteDateRangeAt).
	from, to, err := deletionInterval(firstDate, lastDate, time.Now().UTC())
	if err != nil {
		return DeletePreview{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, MaxQueryDuration)
	defer cancel()
	snapshot, err := q.recorder.querySnapshot(ctx)
	if err != nil {
		return DeletePreview{}, err
	}
	defer snapshot.Close()
	preview := DeletePreview{}
	// Segments mirrors DeleteResult.SegmentsProcessed: every closed segment
	// (raw or compressed) plus the active file, which the deletion rotates
	// into a closed segment first when it holds bytes. An empty active file is
	// never rotated, so it is not counted.
	for _, file := range snapshot.files {
		if file.active && file.size == 0 {
			continue
		}
		preview.Segments++
	}
	// A closed segment is immutable, so its modification time bounds the write
	// time of every record inside it (for a compressed segment it bounds the
	// later compression time, which still postdates every write). A record's
	// started_at never exceeds its write time. Therefore mtime + margin < from
	// proves the segment holds no record with started_at >= from, and it can
	// be skipped without reading or decompressing it. There is no symmetric
	// skip on the upper side: a request started before `to` may keep writing
	// records into later segments, and those continuation records are exactly
	// what the deletion removes.
	skip := func(file queryFile) bool {
		return !file.modTime.IsZero() && file.modTime.Add(previewSkipMargin).Before(from)
	}
	scanErr := snapshot.scanWithSkip(ctx, skip, func(record Record) error {
		if !record.StartedAt.Before(from) && record.StartedAt.Before(to) {
			if preview.Records >= MaxDeletePreviewRecords {
				return errPreviewCapped
			}
			preview.Records++
		}
		return nil
	})
	if errors.Is(scanErr, errPreviewCapped) {
		preview.Capped = true
		preview.Records = MaxDeletePreviewRecords
	} else if scanErr != nil {
		return DeletePreview{}, scanErr
	}
	return preview, nil
}
