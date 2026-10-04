package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// operationGate gives history readers a file lease while allowing recorder writes
// and rotations to continue. Exclusive maintenance prevents new queries at once
// and waits for existing readers to finish without holding Recorder.mu.
type operationGate struct {
	mu          sync.Mutex
	readers     int
	maintenance bool
	changed     chan struct{}
}

func newOperationGate() *operationGate {
	return &operationGate{changed: make(chan struct{})}
}

func (g *operationGate) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *operationGate) acquireQuery(ctx context.Context) (*QueryLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.maintenance {
		return nil, ErrMaintenance
	}
	if g.readers >= MaxQueryReaders {
		return nil, ErrQueryBusy
	}
	g.readers++
	return &QueryLease{gate: g}, nil
}

// BeginMaintenance is the future compression/deletion coordination hook. It
// rejects competing maintenance, refuses new query leases, and honors cancellation
// while existing scans drain. The recorder writer never takes this gate.
func (g *operationGate) beginMaintenance(ctx context.Context) (*MaintenanceLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.maintenance {
		g.mu.Unlock()
		return nil, ErrMaintenance
	}
	g.maintenance = true
	g.signalLocked()
	for g.readers != 0 {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.maintenance = false
			g.signalLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
			g.mu.Lock()
		}
	}
	g.mu.Unlock()
	return &MaintenanceLease{gate: g}, nil
}

// QueryLease is held for the complete scan lifetime.
type QueryLease struct {
	gate *operationGate
	once sync.Once
}

func (l *QueryLease) Close() error {
	if l == nil || l.gate == nil {
		return nil
	}
	l.once.Do(func() {
		l.gate.mu.Lock()
		l.gate.readers--
		l.gate.signalLocked()
		l.gate.mu.Unlock()
	})
	return nil
}

// MaintenanceLease excludes new history queries until Close.
type MaintenanceLease struct {
	gate *operationGate
	once sync.Once
}

func (l *MaintenanceLease) Close() error {
	if l == nil || l.gate == nil {
		return nil
	}
	l.once.Do(func() {
		l.gate.mu.Lock()
		l.gate.maintenance = false
		l.gate.signalLocked()
		l.gate.mu.Unlock()
	})
	return nil
}

// BeginMaintenance starts the exclusive file-operation lease used by later
// compression and deletion tasks. New queries receive ErrMaintenance immediately.
func (r *Recorder) BeginMaintenance(ctx context.Context) (*MaintenanceLease, error) {
	if r == nil || r.queryGate == nil {
		return nil, ErrQueryUnavailable
	}
	return r.queryGate.beginMaintenance(ctx)
}

type queryFile struct {
	name   string
	size   int64
	active bool
}

// QuerySnapshot pins the active inode and a fixed set of closed-file names and
// lengths. Closed files are opened one at a time during scans; the file lease
// prevents later maintenance from replacing or deleting them.
type QuerySnapshot struct {
	recorder       *Recorder
	lease          *QueryLease
	files          []queryFile
	active         *os.File
	activeRequests map[string]RequestSummary
	once           sync.Once
}

func (s *QuerySnapshot) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.once.Do(func() {
		if s.active != nil {
			closeErr = s.active.Close()
		}
		_ = s.lease.Close()
	})
	return closeErr
}

func (r *Recorder) querySnapshot(ctx context.Context) (*QuerySnapshot, error) {
	if r == nil || r.historyDir == "" || r.queryGate == nil {
		return nil, ErrQueryUnavailable
	}
	lease, err := r.queryGate.acquireQuery(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := &QuerySnapshot{recorder: r, lease: lease, activeRequests: make(map[string]RequestSummary)}
	r.mu.Lock()
	segments, listErr := listClosedSegments(r.historyDir)
	if listErr != nil {
		r.mu.Unlock()
		_ = snapshot.Close()
		return nil, fmt.Errorf("list closed history files: %w", listErr)
	}
	for _, segment := range segments {
		snapshot.files = append(snapshot.files, queryFile{name: segment.Name, size: segment.Size})
	}
	activePath := filepath.Join(r.historyDir, activeFileName)
	activeFile, openErr := os.Open(activePath)
	if openErr == nil {
		info, statErr := activeFile.Stat()
		if statErr != nil {
			_ = activeFile.Close()
			r.mu.Unlock()
			_ = snapshot.Close()
			return nil, fmt.Errorf("inspect active history file: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			_ = activeFile.Close()
			r.mu.Unlock()
			_ = snapshot.Close()
			return nil, errors.New("active history path is not a regular file")
		}
		snapshot.active = activeFile
		snapshot.files = append(snapshot.files, queryFile{name: activeFileName, size: info.Size(), active: true})
	} else if !errors.Is(openErr, os.ErrNotExist) {
		r.mu.Unlock()
		_ = snapshot.Close()
		return nil, fmt.Errorf("open active history file: %w", openErr)
	}
	capturedAt := time.Now()
	for id, request := range r.active {
		if request == nil {
			continue
		}
		summary := RequestSummary{
			RequestID: id, StartedAt: request.startedAt, DurationNS: request.elapsedNS(capturedAt), Outcome: "active", Active: true,
			ServiceAccount: request.serviceAccount, Model: request.model, Attempts: request.lastAttempt,
		}
		if request.lastProvider != nil {
			provider := *request.lastProvider
			summary.Provider = &provider
		}
		snapshot.activeRequests[id] = summary
	}
	r.mu.Unlock()
	return snapshot, nil
}

func (s *QuerySnapshot) scan(ctx context.Context, visit func(Record) error) error {
	for _, file := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		var reader io.Reader
		var opened *os.File
		if file.active {
			if s.active == nil {
				continue
			}
			reader = io.NewSectionReader(s.active, 0, file.size)
		} else {
			var err error
			opened, err = os.Open(filepath.Join(s.recorder.historyDir, file.name))
			if err != nil {
				return fmt.Errorf("open history file %s: %w", file.name, err)
			}
			info, statErr := opened.Stat()
			if statErr != nil {
				_ = opened.Close()
				return fmt.Errorf("inspect history file %s: %w", file.name, statErr)
			}
			if !info.Mode().IsRegular() || info.Size() < file.size {
				_ = opened.Close()
				return fmt.Errorf("history file %s changed during a read lease", file.name)
			}
			reader = io.NewSectionReader(opened, 0, file.size)
		}
		err := ReadRecords(ctx, reader, visit)
		if opened != nil {
			closeErr := opened.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return fmt.Errorf("read history file %s: %w", file.name, err)
		}
	}
	return nil
}
