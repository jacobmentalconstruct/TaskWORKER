package journal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"taskworker.local/taskworker/internal/core"
)

const HeaderSize = 64
const FrameSize = 72
const AdmissionBytes int64 = 192 << 20
const HardBytes int64 = 256 << 20

type diskFile interface {
	io.ReaderAt
	io.Writer
	Sync() error
	Truncate(int64) error
	Close() error
}
type index struct {
	offset      int64
	length      uint32
	first, last uint64
}

// Journal owns its OS lock until Close. A failed write/sync poisons this handle.
type Journal struct {
	mu              sync.Mutex
	file            diskFile
	lock            *os.File
	state           core.Recovery
	records         []index
	size            int64
	poisoned        error
	closed          bool
	admission, hard int64
	// RecoveredTailBytes reports bytes removed from an incomplete final frame.
	RecoveredTailBytes int64
}

var _ core.Store = (*Journal)(nil)

func storage(err error) error {
	return &core.Fault{Code: core.ErrStorage, Message: fmt.Sprintf("journal: %v", err)}
}
func limit() error {
	return &core.Fault{Code: core.ErrLimitExceeded, Message: "journal storage budget exhausted; preserve history and use a new data directory"}
}

func Open(dir string) (*Journal, error) {
	// Sync directory entries created by MkdirAll as well as the journal entry.
	var created []string
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		_, err := os.Stat(p)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, storage(err)
		}
		created = append(created, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, storage(err)
	}
	for i := len(created) - 1; i >= 0; i-- {
		if err := syncDirectory(filepath.Dir(created[i])); err != nil {
			return nil, storage(err)
		}
	}
	lock, err := lockDirectory(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return nil, storage(err)
	}
	j := &Journal{lock: lock, admission: AdmissionBytes, hard: HardBytes}
	fail := func(err error) (*Journal, error) { j.Close(); return nil, storage(err) }
	path := filepath.Join(dir, "journal.bin")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	fresh := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR, 0600)
	}
	if err != nil {
		return fail(err)
	}
	j.file = f
	if fresh {
		h := make([]byte, HeaderSize)
		copy(h, []byte("TWJRNL\r\n"))
		binary.BigEndian.PutUint32(h[8:12], 1)
		binary.BigEndian.PutUint32(h[12:16], HeaderSize)
		if _, err = rand.Read(h[16:32]); err != nil {
			return fail(err)
		}
		sum := sha256.Sum256(h[:32])
		copy(h[32:], sum[:])
		if err = writeFull(f, h); err != nil {
			return fail(err)
		}
		if err = f.Sync(); err != nil {
			return fail(err)
		}
		if err = syncDirectory(dir); err != nil {
			return fail(err)
		}
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	j.size = info.Size()
	if j.size < HeaderSize || j.size > j.hard {
		return fail(errors.New("invalid journal file size"))
	}
	h := make([]byte, HeaderSize)
	if _, err = f.ReadAt(h, 0); err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(h[:32])
	if string(h[:8]) != "TWJRNL\r\n" || binary.BigEndian.Uint32(h[8:12]) != 1 || binary.BigEndian.Uint32(h[12:16]) != HeaderSize || !bytes.Equal(h[32:], sum[:]) {
		return fail(errors.New("corrupt or unsupported header"))
	}
	j.state.Snapshot = core.Snapshot{Cursor: core.Cursor{StoreID: hex.EncodeToString(h[16:32])}, Queue: core.QueueState{MaxPending: core.MaxPending}}
	keys := map[string]bool{}
	pos := int64(HeaderSize)
	for pos < j.size {
		if j.size-pos < FrameSize {
			break
		}
		header := make([]byte, FrameSize)
		if _, err = f.ReadAt(header, pos); err != nil {
			return fail(err)
		}
		length := binary.BigEndian.Uint32(header[4:8])
		prefixSum := sha256.Sum256(header[:8])
		if !bytes.Equal(prefixSum[:], header[8:40]) {
			return fail(errors.New("frame header checksum mismatch"))
		}
		if binary.BigEndian.Uint32(header[:4]) != 1 || length == 0 || length > core.MaxCommitBytes {
			return fail(errors.New("unsupported frame or invalid length"))
		}
		if int64(length) > j.size-pos-FrameSize {
			break
		}
		b := make([]byte, length)
		if _, err = f.ReadAt(b, pos+FrameSize); err != nil {
			return fail(err)
		}
		sum = sha256.Sum256(b)
		if !bytes.Equal(sum[:], header[40:]) {
			return fail(errors.New("checksum mismatch"))
		}
		c, err := decode(b)
		if err != nil {
			return fail(err)
		}
		next, err := core.ApplyCommit(j.state.Snapshot, c)
		if err != nil {
			return fail(err)
		}
		if c.Receipt != nil {
			if keys[c.Receipt.Key] {
				return fail(errors.New("duplicate receipt"))
			}
			keys[c.Receipt.Key] = true
			j.state.Receipts = append(j.state.Receipts, *c.Receipt)
		}
		j.records = append(j.records, index{pos, length, c.Events[0].Cursor.Sequence, next.Cursor.Sequence})
		j.state.Snapshot = next
		pos += FrameSize + int64(length)
	}
	if pos != j.size {
		j.RecoveredTailBytes = j.size - pos
		if err = f.Truncate(pos); err != nil {
			return fail(err)
		}
		if err = f.Sync(); err != nil {
			return fail(err)
		}
		j.size = pos
	}
	if _, err = f.Seek(j.size, io.SeekStart); err != nil {
		return fail(err)
	}
	return j, nil
}

func decode(b []byte) (core.Commit, error) {
	var c core.Commit
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("trailing JSON")
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, b) {
		return c, errors.New("noncanonical transaction JSON")
	}
	return c, nil
}
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (j *Journal) check() error {
	if j.poisoned != nil {
		return j.poisoned
	}
	if j.closed {
		return storage(errors.New("closed"))
	}
	return nil
}

func (j *Journal) readFailure(err error) error {
	j.poisoned = storage(err)
	return j.poisoned
}
func (j *Journal) Recover(ctx context.Context) (core.Recovery, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return core.Recovery{}, err
	}
	if err := j.check(); err != nil {
		return core.Recovery{}, err
	}
	return core.Clone(j.state), nil
}
func (j *Journal) Append(ctx context.Context, c core.Commit) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := j.check(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return storage(err)
	}
	if len(b) > core.MaxCommitBytes {
		return limit()
	}
	next, err := core.ApplyCommit(j.state.Snapshot, c)
	if err != nil {
		return err
	}
	if c.Receipt != nil {
		for _, r := range j.state.Receipts {
			if r.Key == c.Receipt.Key {
				return storage(errors.New("duplicate receipt"))
			}
		}
	}
	projected := j.size + FrameSize + int64(len(b))
	if (c.Receipt != nil && projected > j.admission) || projected+core.TerminalReserve(next) > j.hard {
		return limit()
	}
	h := make([]byte, FrameSize)
	binary.BigEndian.PutUint32(h[:4], 1)
	binary.BigEndian.PutUint32(h[4:8], uint32(len(b)))
	prefixSum := sha256.Sum256(h[:8])
	copy(h[8:40], prefixSum[:])
	sum := sha256.Sum256(b)
	copy(h[40:], sum[:])
	if err = writeFull(j.file, append(h, b...)); err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.poisoned = storage(err)
		return j.poisoned
	}
	j.records = append(j.records, index{j.size, uint32(len(b)), c.Events[0].Cursor.Sequence, next.Cursor.Sequence})
	j.size = projected
	j.state.Snapshot = next
	if c.Receipt != nil {
		j.state.Receipts = append(j.state.Receipts, *c.Receipt)
	}
	return nil
}
func (j *Journal) ReadEvents(ctx context.Context, after core.Cursor, limit int) ([]core.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := j.check(); err != nil {
		return nil, err
	}
	if err := core.ValidateCursor(after, j.state.Snapshot.Cursor); err != nil {
		return nil, err
	}
	if limit < 1 || limit > core.MaxObserverEvents {
		return nil, &core.Fault{Code: core.ErrInvalidRequest, Message: "invalid replay page size"}
	}
	out := []core.Event{}
	bytesReturned := 0
	i := sort.Search(len(j.records), func(i int) bool { return j.records[i].last > after.Sequence })
	for ; i < len(j.records) && len(out) < limit; i++ {
		r := j.records[i]
		b := make([]byte, r.length)
		if _, err := j.file.ReadAt(b, r.offset+FrameSize); err != nil {
			return nil, j.readFailure(err)
		}
		header := make([]byte, FrameSize)
		if _, err := j.file.ReadAt(header, r.offset); err != nil {
			return nil, j.readFailure(err)
		}
		sum := sha256.Sum256(b)
		prefixSum := sha256.Sum256(header[:8])
		if binary.BigEndian.Uint32(header[:4]) != 1 || binary.BigEndian.Uint32(header[4:8]) != r.length || !bytes.Equal(prefixSum[:], header[8:40]) || !bytes.Equal(sum[:], header[40:]) {
			return nil, j.readFailure(errors.New("replay frame changed"))
		}
		c, err := decode(b)
		if err != nil {
			return nil, j.readFailure(err)
		}
		for _, e := range c.Events {
			if e.Cursor.Sequence > after.Sequence {
				encoded, _ := json.Marshal(e)
				if bytesReturned+len(encoded) > core.MaxObserverBytes {
					return out, nil
				}
				bytesReturned += len(encoded)
				out = append(out, e)
				if len(out) == limit {
					break
				}
			}
		}
	}
	return out, nil
}
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	var err error
	if j.file != nil {
		err = j.file.Close()
	}
	if j.lock != nil {
		err = errors.Join(err, j.lock.Close())
	}
	return err
}
