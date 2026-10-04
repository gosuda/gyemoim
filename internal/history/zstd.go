package history

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	zstdCommandName      = "zstd"
	compressedSuffix     = ".zst"
	compressedTempMarker = ".tmp-"
	maxZstdStderrBytes   = 4096
	zstdWaitDelay        = 2 * time.Second
)

var (
	ErrZstdUnavailable          = errors.New("zstd executable is unavailable")
	ErrCompressedHistoryInvalid = errors.New("compressed history is invalid")
)

// boundedBuffer retains only the end of a child process's stderr. Stderr is
// diagnostic input and is never exposed through storage status.
type boundedBuffer struct {
	data []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) >= maxZstdStderrBytes {
		b.data = append(b.data[:0], p[len(p)-maxZstdStderrBytes:]...)
		return original, nil
	}
	if excess := len(b.data) + len(p) - maxZstdStderrBytes; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, p...)
	return original, nil
}

type zstdReader struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	cancel context.CancelFunc
	stderr boundedBuffer
	once   sync.Once
	err    error
}

func startZstdReader(ctx context.Context, executable string, input *os.File) (*zstdReader, error) {
	if executable == "" {
		return nil, ErrZstdUnavailable
	}
	if err := ensureZstdExecutable(executable); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, executable, "-q", "-dc")
	cmd.Stdin = input
	cmd.WaitDelay = zstdWaitDelay
	reader := &zstdReader{cmd: cmd, cancel: cancel}
	cmd.Stderr = &reader.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		cancel()
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrZstdUnavailable
		}
		return nil, err
	}
	reader.stdout = stdout
	return reader, nil
}

func (r *zstdReader) Read(p []byte) (int, error) { return r.stdout.Read(p) }

// Finish always closes the pipe and waits for the child. A visitor error or
// cancellation stops the child before Wait, so an early query exit cannot leave
// a decompressor blocked on stdout.
func (r *zstdReader) Finish(readErr error) error {
	if r == nil {
		return readErr
	}
	r.once.Do(func() {
		if readErr != nil {
			r.cancel()
		}
		if r.stdout != nil {
			_ = r.stdout.Close()
		}
		waitErr := r.cmd.Wait()
		r.cancel()
		if readErr != nil && waitErr != nil {
			r.err = errors.Join(readErr, fmt.Errorf("zstd decompression process failed: %w", waitErr))
		} else if readErr != nil {
			r.err = readErr
		} else if waitErr != nil {
			r.err = fmt.Errorf("zstd decompression process failed: %w", waitErr)
		}
	})
	return r.err
}

func resolveZstdExecutable() string {
	path, err := exec.LookPath(zstdCommandName)
	if err != nil {
		return ""
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absPath
}

func openRegularFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("history path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, errors.New("history path changed while opening")
	}
	return file, nil
}

func validateCompressedHistory(ctx context.Context, executable, path string, visit func(Record) error) ([sha256.Size]byte, int64, error) {
	var zero [sha256.Size]byte
	if executable == "" {
		return zero, 0, ErrZstdUnavailable
	}
	input, err := openRegularFile(path)
	if err != nil {
		return zero, 0, err
	}
	defer input.Close()
	process, err := startZstdReader(ctx, executable, input)
	if err != nil {
		return zero, 0, err
	}
	digest := sha256.New()
	var byteCount int64
	tee := io.TeeReader(process, io.MultiWriter(digest, counterWriter{count: &byteCount}))
	if visit == nil {
		visit = func(Record) error { return nil }
	}
	_, readErr := walkRecords(ctx, tee, false, visit)
	finishErr := process.Finish(readErr)
	if finishErr != nil {
		return zero, 0, finishErr
	}
	var sum [sha256.Size]byte
	copy(sum[:], digest.Sum(nil))
	return sum, byteCount, nil
}

type counterWriter struct{ count *int64 }

func (w counterWriter) Write(p []byte) (int, error) {
	*w.count += int64(len(p))
	return len(p), nil
}

func hashRegularFile(ctx context.Context, path string) ([sha256.Size]byte, int64, error) {
	var zero [sha256.Size]byte
	file, err := openRegularFile(path)
	if err != nil {
		return zero, 0, err
	}
	defer file.Close()
	digest := sha256.New()
	var count int64
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return zero, count, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			written, writeErr := digest.Write(buffer[:n])
			count += int64(written)
			if writeErr != nil {
				return zero, count, writeErr
			}
			if written != n {
				return zero, count, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return zero, count, readErr
		}
		if n == 0 {
			return zero, count, io.ErrNoProgress
		}
	}
	var sum [sha256.Size]byte
	copy(sum[:], digest.Sum(nil))
	return sum, count, nil
}

func runZstdCompression(ctx context.Context, executable string, input, output *os.File) error {
	if executable == "" {
		return ErrZstdUnavailable
	}
	if err := ensureZstdExecutable(executable); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, executable, "-q", "-c")
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.WaitDelay = zstdWaitDelay
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return ErrZstdUnavailable
		}
		return fmt.Errorf("zstd compression process failed: %w", err)
	}
	return nil
}

func ensureZstdExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ErrZstdUnavailable
	}
	return nil
}
