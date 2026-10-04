package history

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	completedIDWidth      = maxRequestID
	completedIDChunkBytes = 8 << 20
	completedIDMaxRuns    = 4096
	// Keep total merge descriptors at 32: at most 31 input runs and one output.
	completedIDMergeFanIn = 31
)

// completedIDIndex stores exact schema 2 end IDs for the interrupted-start scan.
// Small snapshots stay in memory. Larger snapshots spill sorted, fixed-width runs
// into one private directory owned by this query.
type completedIDIndex struct {
	chunk     []byte
	runs      []string
	tempDir   string
	nextRun   uint64
	indexFile *os.File
	count     int64
	finalized bool
}

func newCompletedIDIndex() *completedIDIndex {
	return &completedIDIndex{}
}

func (index *completedIDIndex) add(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRequestID(id) || len(id) > completedIDWidth {
		return fmt.Errorf("invalid request ID in completed history record")
	}
	if len(index.chunk)+completedIDWidth > completedIDChunkBytes {
		if err := index.flushRun(ctx); err != nil {
			return err
		}
	}
	start := len(index.chunk)
	index.growChunk(start + completedIDWidth)
	index.chunk = index.chunk[:start+completedIDWidth]
	for position := start; position < start+completedIDWidth; position++ {
		index.chunk[position] = 0
	}
	copy(index.chunk[start:start+completedIDWidth], id)
	return nil
}

func (index *completedIDIndex) growChunk(required int) {
	if cap(index.chunk) >= required {
		return
	}
	capacity := cap(index.chunk) * 2
	if capacity < 256*completedIDWidth {
		capacity = 256 * completedIDWidth
	}
	if capacity > completedIDChunkBytes {
		capacity = completedIDChunkBytes
	}
	if capacity < required {
		capacity = required
	}
	data := make([]byte, len(index.chunk), capacity)
	copy(data, index.chunk)
	index.chunk = data
}

func (index *completedIDIndex) finalize(ctx context.Context) error {
	if index.finalized {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(index.runs) == 0 {
		index.chunk = sortedUniqueCompletedIDs(index.chunk)
		if err := ctx.Err(); err != nil {
			return err
		}
		index.count = int64(len(index.chunk) / completedIDWidth)
		index.finalized = true
		return nil
	}
	if err := index.flushRun(ctx); err != nil {
		return err
	}
	index.chunk = nil
	runs := append([]string(nil), index.runs...)
	for len(runs) > completedIDMergeFanIn {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := make([]string, 0, (len(runs)+completedIDMergeFanIn-1)/completedIDMergeFanIn)
		for first := 0; first < len(runs); first += completedIDMergeFanIn {
			if err := ctx.Err(); err != nil {
				return err
			}
			last := first + completedIDMergeFanIn
			if last > len(runs) {
				last = len(runs)
			}
			group := runs[first:last]
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}
			output, err := index.newRunPath()
			if err != nil {
				return err
			}
			_, err = mergeCompletedIDRuns(ctx, group, output)
			if err != nil {
				return err
			}
			if err := removeCompletedIDRuns(ctx, group); err != nil {
				return err
			}
			next = append(next, output)
		}
		runs = next
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	output, err := index.newRunPath()
	if err != nil {
		return err
	}
	count, err := mergeCompletedIDRuns(ctx, runs, output)
	if err != nil {
		return err
	}
	if err := removeCompletedIDRuns(ctx, runs); err != nil {
		return err
	}
	file, err := os.Open(output)
	if err != nil {
		return fmt.Errorf("open completed-request query index: %w", err)
	}
	index.indexFile = file
	index.count = count
	index.chunk = nil
	index.runs = nil
	index.finalized = true
	return nil
}

func (index *completedIDIndex) contains(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !validRequestID(id) || len(id) > completedIDWidth {
		return false, fmt.Errorf("invalid request ID in request-start history record")
	}
	var key [completedIDWidth]byte
	copy(key[:], id)
	var slot [completedIDWidth]byte
	low, high := int64(0), index.count
	for low < high {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		middle := low + (high-low)/2
		if index.indexFile == nil {
			start := int(middle) * completedIDWidth
			copy(slot[:], index.chunk[start:start+completedIDWidth])
		} else {
			n, err := index.indexFile.ReadAt(slot[:], middle*completedIDWidth)
			if err != nil || n != completedIDWidth {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return false, fmt.Errorf("read completed-request query index: %w", err)
			}
		}
		if bytes.Compare(slot[:], key[:]) < 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low >= index.count {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if index.indexFile == nil {
		start := int(low) * completedIDWidth
		copy(slot[:], index.chunk[start:start+completedIDWidth])
	} else {
		n, err := index.indexFile.ReadAt(slot[:], low*completedIDWidth)
		if err != nil || n != completedIDWidth {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return false, fmt.Errorf("read completed-request query index: %w", err)
		}
	}
	return bytes.Equal(slot[:], key[:]), nil
}

func (index *completedIDIndex) flushRun(ctx context.Context) error {
	if len(index.chunk) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	index.chunk = sortedUniqueCompletedIDs(index.chunk)
	if err := ctx.Err(); err != nil {
		return err
	}
	if index.tempDir == "" {
		dir, err := os.MkdirTemp("", "gyemoim-query-*")
		if err != nil {
			return fmt.Errorf("create completed-request query index directory: %w", err)
		}
		index.tempDir = dir
	}
	if len(index.runs) >= completedIDMaxRuns {
		return fmt.Errorf("completed-request query index exceeds %d temporary runs", completedIDMaxRuns)
	}
	path, err := index.newRunPath()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create completed-request query index run: %w", err)
	}
	writeErr := writeCompletedIDBytes(ctx, file, index.chunk)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(wrapCompletedIDError("write completed-request query index run", writeErr), wrapCompletedIDError("close completed-request query index run", closeErr))
	}
	index.runs = append(index.runs, path)
	index.chunk = index.chunk[:0]
	return nil
}

func (index *completedIDIndex) newRunPath() (string, error) {
	if index.tempDir == "" {
		dir, err := os.MkdirTemp("", "gyemoim-query-*")
		if err != nil {
			return "", fmt.Errorf("create completed-request query index directory: %w", err)
		}
		index.tempDir = dir
	}
	path := filepath.Join(index.tempDir, fmt.Sprintf("run-%08d.bin", index.nextRun))
	index.nextRun++
	return path, nil
}

func (index *completedIDIndex) close() error {
	var closeErr error
	if index.indexFile != nil {
		closeErr = index.indexFile.Close()
		index.indexFile = nil
	}
	if index.tempDir != "" {
		if err := os.RemoveAll(index.tempDir); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("remove completed-request query index directory: %w", err))
		}
		index.tempDir = ""
	}
	return closeErr
}

func sortedUniqueCompletedIDs(data []byte) []byte {
	sort.Sort(completedIDSlots(data))
	count := len(data) / completedIDWidth
	unique := 0
	for read := 0; read < count; read++ {
		start := read * completedIDWidth
		if unique > 0 && bytes.Equal(data[(unique-1)*completedIDWidth:unique*completedIDWidth], data[start:start+completedIDWidth]) {
			continue
		}
		if unique != read {
			copy(data[unique*completedIDWidth:(unique+1)*completedIDWidth], data[start:start+completedIDWidth])
		}
		unique++
	}
	return data[:unique*completedIDWidth]
}

type completedIDSlots []byte

func (slots completedIDSlots) Len() int { return len(slots) / completedIDWidth }
func (slots completedIDSlots) Less(left, right int) bool {
	leftStart, rightStart := left*completedIDWidth, right*completedIDWidth
	return bytes.Compare(slots[leftStart:leftStart+completedIDWidth], slots[rightStart:rightStart+completedIDWidth]) < 0
}
func (slots completedIDSlots) Swap(left, right int) {
	if left == right {
		return
	}
	leftStart, rightStart := left*completedIDWidth, right*completedIDWidth
	var temporary [completedIDWidth]byte
	copy(temporary[:], slots[leftStart:leftStart+completedIDWidth])
	copy(slots[leftStart:leftStart+completedIDWidth], slots[rightStart:rightStart+completedIDWidth])
	copy(slots[rightStart:rightStart+completedIDWidth], temporary[:])
}

func writeCompletedIDBytes(ctx context.Context, file *os.File, data []byte) error {
	const writeChunk = 64 << 10
	writer := bufio.NewWriterSize(file, writeChunk)
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + writeChunk
		if end > len(data) {
			end = len(data)
		}
		if _, err := writer.Write(data[offset:end]); err != nil {
			return err
		}
		offset = end
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writer.Flush()
}

type completedIDRunHead struct {
	reader *bufio.Reader
	value  [completedIDWidth]byte
	index  int
}

type completedIDRunHeap []*completedIDRunHead

func (heads completedIDRunHeap) Len() int { return len(heads) }
func (heads completedIDRunHeap) Less(left, right int) bool {
	if comparison := bytes.Compare(heads[left].value[:], heads[right].value[:]); comparison != 0 {
		return comparison < 0
	}
	return heads[left].index < heads[right].index
}
func (heads completedIDRunHeap) Swap(left, right int) {
	heads[left], heads[right] = heads[right], heads[left]
}
func (heads *completedIDRunHeap) Push(value any) {
	*heads = append(*heads, value.(*completedIDRunHead))
}
func (heads *completedIDRunHeap) Pop() any {
	previous := *heads
	last := previous[len(previous)-1]
	*heads = previous[:len(previous)-1]
	return last
}

func mergeCompletedIDRuns(ctx context.Context, paths []string, output string) (count int64, retErr error) {
	if len(paths) > completedIDMergeFanIn {
		return 0, fmt.Errorf("completed-request query index merge fan-in %d exceeds %d", len(paths), completedIDMergeFanIn)
	}
	files := make([]*os.File, 0, len(paths))
	closeFiles := func() error {
		var closeErr error
		for _, file := range files {
			if err := file.Close(); err != nil {
				closeErr = errors.Join(closeErr, err)
			}
		}
		return closeErr
	}
	defer func() {
		if err := closeFiles(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close completed-request query index merge input: %w", err))
		}
	}()
	heads := make(completedIDRunHeap, 0, len(paths))
	for position, path := range paths {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		file, err := os.Open(path)
		if err != nil {
			return 0, fmt.Errorf("open completed-request query index run: %w", err)
		}
		files = append(files, file)
		info, err := file.Stat()
		if err != nil {
			return 0, fmt.Errorf("inspect completed-request query index run: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size()%completedIDWidth != 0 {
			return 0, errors.New("completed-request query index run has an invalid size")
		}
		head := &completedIDRunHead{reader: bufio.NewReaderSize(file, 16<<10), index: position}
		readErr := readCompletedIDHead(head)
		if readErr != nil {
			return 0, fmt.Errorf("read completed-request query index run: %w", readErr)
		}
		if readErr == nil && head.hasValue() {
			heads = append(heads, head)
		}
	}
	heap.Init(&heads)
	out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, fmt.Errorf("create completed-request query index merge output: %w", err)
	}
	writer := bufio.NewWriterSize(out, 64<<10)
	var previous [completedIDWidth]byte
	havePrevious := false
	for heads.Len() > 0 {
		if err := ctx.Err(); err != nil {
			retErr = err
			break
		}
		head := heap.Pop(&heads).(*completedIDRunHead)
		if !havePrevious || !bytes.Equal(previous[:], head.value[:]) {
			if _, err := writer.Write(head.value[:]); err != nil {
				retErr = fmt.Errorf("write completed-request query index merge output: %w", err)
				break
			}
			previous = head.value
			havePrevious = true
			count++
		}
		readErr := readCompletedIDHead(head)
		if readErr != nil {
			retErr = fmt.Errorf("read completed-request query index run: %w", readErr)
			break
		}
		if head.hasValue() {
			heap.Push(&heads, head)
		}
	}
	if retErr == nil {
		if err := ctx.Err(); err != nil {
			retErr = err
		} else if err := writer.Flush(); err != nil {
			retErr = fmt.Errorf("flush completed-request query index merge output: %w", err)
		}
	}
	closeErr := out.Close()
	if closeErr != nil {
		retErr = errors.Join(retErr, fmt.Errorf("close completed-request query index merge output: %w", closeErr))
	}
	if retErr != nil {
		return 0, retErr
	}
	return count, nil
}

func (head *completedIDRunHead) hasValue() bool { return head.reader != nil }

func readCompletedIDHead(head *completedIDRunHead) error {
	if head.reader == nil {
		return nil
	}
	_, err := io.ReadFull(head.reader, head.value[:])
	if err == io.EOF {
		head.reader = nil
		return nil
	}
	if err != nil {
		return err
	}
	return nil
}

func removeCompletedIDRuns(ctx context.Context, paths []string) error {
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove completed-request query index run: %w", err)
		}
	}
	return nil
}

func wrapCompletedIDError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}
