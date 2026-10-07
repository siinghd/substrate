// Package snapstore implements the content-addressed chunk store behind
// snapshots and rootfs images (plan §3.2): 512KB sha256-addressed chunks,
// zero-page elision, a hotness-ordered index per snapshot, an LRU-bounded
// node-local NVMe cache, and a pluggable object-storage demotion tier.
package snapstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const DefaultChunkSize = 512 << 10 // 512KB, matching Lambda's documented design

var (
	ErrChunkMissing = errors.New("snapstore: chunk not in cache or origin")
	// ErrCacheTooSmall is returned when a blob's own chunks exceed the whole
	// cache budget, so it could never be held resident.
	ErrCacheTooSmall = errors.New("snapstore: blob exceeds the chunk cache budget")
)

// ChunkRef describes one chunk of a snapshot blob. Hash == "" means the range
// is implicitly zeros (zero-page elision: never stored, never resident).
type ChunkRef struct {
	Hash string `json:"h,omitempty"`
	Len  int    `json:"len"`
}

// Index is the restore map for one snapshot blob: ordered chunk refs plus the
// hotness order recorded during template warm-runs / previous restores.
type Index struct {
	ID        string     `json:"id"`
	TotalSize int64      `json:"total_size"`
	ChunkSize int        `json:"chunk_size"`
	Chunks    []ChunkRef `json:"chunks"`
	Hot       []int      `json:"hot"` // indices into Chunks, hottest first
}

// HotBytes returns how many bytes the eager-install hot set covers.
func (ix *Index) HotBytes() int64 {
	var n int64
	for _, i := range ix.Hot {
		n += int64(ix.Chunks[i].Len)
	}
	return n
}

type ObjectStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Store is the node-local chunk cache with LRU eviction and optional
// object-storage origin. Safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	root      string
	chunkSize int
	maxBytes  int64
	curBytes  int64
	lru       list
	items     map[string]*entry
	origin    ObjectStore

	// pins protects chunks from eviction while something still needs them.
	//
	// HONESTY NOTE: the corruption this was reported against is actually fixed
	// by the budget check in PutBlob, not by the pins -- with the check removed
	// but pins kept, the corruption still reproduces, and with the check kept
	// but pins removed it does not, at any budget tested. The pins are
	// defensive depth for when chunks start being LRU-tracked as they are
	// written (today they are inserted only after the read loop, which is its
	// own accounting gap: a multi-GB build writes to disk without curBytes
	// knowing). Keeping them is cheap; claiming they carry the fix would not be
	// true.
	pins map[string]int
}

type entry struct {
	key  string
	size int64
	el   *element
}

type element struct {
	key        string
	prev, next *element
}

type list struct{ head, tail *element }

func (l *list) pushFront(e *element) {
	e.next = l.head
	e.prev = nil
	if l.head != nil {
		l.head.prev = e
	}
	l.head = e
	if l.tail == nil {
		l.tail = e
	}
}

func (l *list) remove(e *element) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
}

func (l *list) moveToFront(e *element) {
	l.remove(e)
	l.pushFront(e)
}

func New(root string, maxCacheBytes int64, origin ObjectStore) (*Store, error) {
	s := &Store{
		root:      root,
		chunkSize: DefaultChunkSize,
		maxBytes:  maxCacheBytes,
		items:     map[string]*entry{},
		origin:    origin,
	}
	for _, d := range []string{s.chunksDir(), s.indexDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.rebuildLRU(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) chunksDir() string { return filepath.Join(s.root, "chunks") }
func (s *Store) indexDir() string  { return filepath.Join(s.root, "indexes") }

// rebuildLRU indexes on-disk chunks. Only valid sha256 hex basenames are
// tracked: stray files (partial writes, *.tmp leftovers from a crash) would
// otherwise panic chunkPath via hash[:2] on names shorter than 2 bytes.
func (s *Store) rebuildLRU() error {
	return filepath.Walk(s.chunksDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		hash := filepath.Base(path)
		if strings.HasSuffix(hash, ".tmp") {
			// A leftover from a writer that died. No writer survives a process
			// restart, so this is unconditionally garbage. Left in place they
			// accumulated forever: invisible to Stats(), never evicted, and
			// eventually filling the disk while the budget looked healthy.
			_ = os.Remove(path)
			return nil
		}
		if len(hash) != sha256.Size*2 || strings.TrimLeft(hash, "0123456789abcdef") != "" {
			// Not a chunk we can address, but it is still occupying the budget
			// we are supposed to be managing. Count it so eviction reflects
			// real disk usage instead of an optimistic fiction.
			s.curBytes += info.Size()
			return nil
		}
		e := &entry{key: hash, size: info.Size()}
		s.lru.pushFront(&element{key: hash})
		e.el = s.lru.head
		s.items[hash] = e
		s.curBytes += info.Size()
		return nil
	})
}

func chunkPath(root, hash string) string {
	return filepath.Join(root, "chunks", hash[:2], hash)
}

func (s *Store) has(hash string) bool {
	_, ok := s.items[hash]
	return ok
}

// hasLocked is has() for callers already holding s.mu — and the ONLY safe
// form when another goroutine may be mutating the index concurrently
// (PutBlob's streaming dedup check). The unlocked has() must only be called
// from sections that provably hold s.mu.
func (s *Store) hasLocked(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.has(hash)
}

// dropLocked forgets a chunk we can no longer read. Callers must hold s.mu.
func (s *Store) dropLocked(hash string) {
	e, ok := s.items[hash]
	if !ok {
		return
	}
	s.lru.remove(e.el)
	delete(s.items, hash)
	s.curBytes -= e.size
	if s.curBytes < 0 {
		s.curBytes = 0
	}
}

// pin / unpin mark chunks as in-use. Callers must hold s.mu.
func (s *Store) pin(hash string) {
	if s.pins == nil {
		s.pins = map[string]int{}
	}
	s.pins[hash]++
}

func (s *Store) unpin(hash string) {
	if n := s.pins[hash]; n > 1 {
		s.pins[hash] = n - 1
	} else {
		delete(s.pins, hash)
	}
}

// touch marks a chunk recently used and evicts the LRU tail while over budget.
//
// Pinned chunks are skipped, never deleted: eviction walks toward the head past
// anything in use. If everything left is pinned the loop simply stops — being
// over budget is a capacity problem, whereas deleting a chunk someone is still
// assembling is silent data loss.
func (s *Store) touch(hash string) {
	if e, ok := s.items[hash]; ok {
		s.lru.moveToFront(e.el)
	}
	s.evictLocked()
}

func (s *Store) evictLocked() {
	cur := s.lru.tail
	for s.curBytes > s.maxBytes && cur != nil {
		victim := cur.key
		prev := cur.prev
		if _, held := s.pins[victim]; held {
			cur = prev // in use; look further toward the head
			continue
		}
		v, ok := s.items[victim]
		if !ok {
			cur = prev
			continue
		}
		s.lru.remove(v.el)
		delete(s.items, victim)
		s.curBytes -= v.size
		_ = os.Remove(chunkPath(s.root, victim))
		cur = prev
	}
}

// PutBlob chunks r, stores new chunks (dedup: existing hashes are skipped),
// and returns the index. All-zero chunks are elided entirely.
//
// Chunks stream to disk as they are read: the previous version buffered the
// entire deduped blob in RAM before writing anything, so a multi-GB rootfs
// upload spiked host memory by the full input size. Peak memory is now
// O(chunkSize) regardless of blob size; disk writes still happen outside the
// lock and concurrent writers are safe (content-addressed + unique tmp files).
func (s *Store) PutBlob(ctx context.Context, id string, r io.Reader) (*Index, error) {
	ix := &Index{ID: id, ChunkSize: s.chunkSize}
	buf := make([]byte, s.chunkSize)
	var total int64
	written := map[string]int64{} // chunks this call wrote: hash -> size
	var pinnedHere []string
	defer func() {
		s.mu.Lock()
		for _, h := range pinnedHere {
			s.unpin(h)
		}
		// Now that nothing is pinned by this call, bring the cache back within
		// budget in the normal way.
		s.evictLocked()
		s.mu.Unlock()
	}()
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			chunk := buf[:n]
			total += int64(n)
			if isZero(chunk) {
				ix.Chunks = append(ix.Chunks, ChunkRef{Len: n})
			} else {
				sum := sha256.Sum256(chunk)
				hash := hex.EncodeToString(sum[:])
				ix.Chunks = append(ix.Chunks, ChunkRef{Hash: hash, Len: n})
				if _, dup := written[hash]; !dup && !s.hasLocked(hash) {
					if err := writeChunk(s.root, hash, chunk); err != nil {
						return nil, err
					}
					written[hash] = int64(n)
				}
				// Pin every chunk this blob references -- including ones that
				// already existed -- for as long as we are assembling it.
				s.mu.Lock()
				s.pin(hash)
				s.mu.Unlock()
				pinnedHere = append(pinnedHere, hash)
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	ix.TotalSize = total

	s.mu.Lock()
	for hash, size := range written {
		if s.has(hash) {
			continue // lost a race; file already tracked
		}
		e := &entry{key: hash, size: size}
		el := &element{key: hash}
		s.lru.pushFront(el)
		e.el = el
		s.items[hash] = e
		s.curBytes += size
	}
	// hot set defaults to everything non-zero, hottest-first = insertion order;
	// warm-run fault recording refines this via RecordHotness.
	ix.Hot = make([]int, 0, len(ix.Chunks))
	for i := range ix.Chunks {
		if ix.Chunks[i].Hash != "" {
			ix.Hot = append(ix.Hot, i)
		}
	}
	for _, i := range ix.Hot {
		s.touch(ix.Chunks[i].Hash)
	}
	// A blob whose own chunks exceed the entire cache budget can never be held
	// resident. Say so instead of returning an index whose chunks will be
	// evicted the moment the pins drop.
	var ownBytes int64
	for _, c := range ix.Chunks {
		if c.Hash != "" {
			ownBytes += int64(c.Len)
		}
	}
	overBudget := ownBytes > s.maxBytes
	s.mu.Unlock()
	if overBudget {
		return nil, fmt.Errorf("%w: blob %s needs %d bytes of chunk cache but the budget is %d",
			ErrCacheTooSmall, id, ownBytes, s.maxBytes)
	}

	if err := s.SaveIndex(ix); err != nil {
		return nil, err
	}
	return ix, nil
}

func writeChunk(root, hash string, data []byte) error {
	p := chunkPath(root, hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Unique temp file per writer: two concurrent PutBlobs storing the same
	// new chunk used to share one `chunk.tmp` inode, so a concurrent
	// truncate/rename could publish a half-written chunk under its
	// content-addressed name — silent, permanent corruption of that chunk.
	tmp, err := os.CreateTemp(filepath.Dir(p), hash+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush the DATA before publishing the name. Without this, ext4's delayed
	// allocation gives the classic rename-without-fsync outcome after a power
	// cut: a zero-length or stale-block file sitting under a valid
	// content-addressed name. rebuildLRU would then index it as a good chunk,
	// ReadBlob would return a short blob with a nil error, and because the
	// store is deduped, every template sharing that chunk inherits it.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return err
	}
	// And flush the DIRECTORY, or the rename itself can be lost while the data
	// survives -- leaving the chunk present only under its tmp name.
	if dir, err := os.Open(filepath.Dir(p)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// ReadBlob reassembles the snapshot blob described by ix. Missing chunks are
// fetched from origin (object storage) if configured.
// WriteBlobTo streams a blob to w. Peak memory is ONE chunk, not the whole
// blob: a 512MiB snapshot no longer needs 512MiB of heap (plus a transient
// copy of every chunk) to be read back. Cached chunks are copied file→writer
// with no intermediate buffer at all.
//
// Complexity: O(chunks) syscalls, O(chunkSize) memory — independent of blob
// size. Zero-elided chunks cost no I/O.
func (s *Store) WriteBlobTo(ctx context.Context, ix *Index, w io.Writer) (int64, error) {
	var written int64
	var zeros []byte
	for i, c := range ix.Chunks {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if c.Hash == "" {
			// Zero-elided: emit from one reusable buffer rather than
			// allocating c.Len bytes per hole.
			if len(zeros) < c.Len {
				zeros = make([]byte, c.Len)
			}
			n, err := w.Write(zeros[:c.Len])
			written += int64(n)
			if err != nil {
				return written, err
			}
			continue
		}
		n, err := s.copyChunkTo(ctx, w, c.Hash, int64(i))
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// copyChunkTo streams one chunk. The cached path avoids materialising it.
func (s *Store) copyChunkTo(ctx context.Context, w io.Writer, hash string, idx int64) (int64, error) {
	s.mu.Lock()
	cached := s.has(hash)
	s.mu.Unlock()
	if cached {
		f, err := os.Open(chunkPath(s.root, hash))
		if err == nil {
			defer f.Close()
			n, cerr := io.Copy(w, f)
			if cerr == nil {
				s.mu.Lock()
				s.touch(hash)
				s.mu.Unlock()
				return n, nil
			}
			return n, cerr
		}
	}
	// Miss: fall back to the buffering path, which also populates the cache.
	data, err := s.getChunk(ctx, hash, idx)
	if err != nil {
		return 0, err
	}
	n, werr := w.Write(data)
	return int64(n), werr
}

// ReadBlob materialises a whole blob. Prefer WriteBlobTo on any path where the
// blob can be large; this exists for callers that genuinely need one slice.
func (s *Store) ReadBlob(ctx context.Context, ix *Index) ([]byte, error) {
	buf := make([]byte, 0, ix.TotalSize)
	w := &sliceWriter{buf: buf}
	if _, err := s.WriteBlobTo(ctx, ix, w); err != nil {
		return nil, err
	}
	return w.buf, nil
}

// sliceWriter appends into a pre-sized slice, so the exact-size allocation
// made up front is the only one.
type sliceWriter struct{ buf []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// ReadRange reads [offset, offset+length) without materializing the whole blob.
func (s *Store) ReadRange(ctx context.Context, ix *Index, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	out := make([]byte, 0, length)
	pos := int64(0)
	for _, c := range ix.Chunks {
		cEnd := pos + int64(c.Len)
		if cEnd > offset && pos < offset+length {
			start := offset - pos
			if start < 0 {
				start = 0
			}
			end := offset + length - pos
			if end > int64(c.Len) {
				end = int64(c.Len)
			}
			if c.Hash == "" {
				out = append(out, make([]byte, end-start)...)
			} else {
				data, err := s.getChunk(ctx, c.Hash, -1)
				if err != nil {
					return nil, err
				}
				out = append(out, data[start:end]...)
			}
		}
		pos = cEnd
		if pos >= offset+length {
			break
		}
	}
	return out, nil
}

func (s *Store) getChunk(ctx context.Context, hash string, _ int64) ([]byte, error) {
	s.mu.Lock()
	cached := s.has(hash)
	s.mu.Unlock()

	p := chunkPath(s.root, hash)
	if cached {
		data, err := os.ReadFile(p)
		if err == nil {
			s.mu.Lock()
			s.touch(hash)
			s.mu.Unlock()
			return data, nil
		}
		// The index says we have it and the file does not exist (or cannot be
		// read). Keeping the entry meant curBytes stayed charged for bytes that
		// are not on disk, and the budget slowly filled with phantoms whose
		// presence evicted real chunks.
		s.mu.Lock()
		s.dropLocked(hash)
		s.mu.Unlock()
	}
	if s.origin != nil {
		rc, err := s.origin.Get(ctx, "chunks/"+hash)
		if err == nil {
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err == nil {
				// The origin is not trusted to return what we asked for: this
				// store is content-addressed, so verify before adopting the
				// bytes under that name. A mismatch adopted here would poison
				// every template that shares the chunk.
				sum := sha256.Sum256(data)
				if hex.EncodeToString(sum[:]) != hash {
					return nil, fmt.Errorf("%w: origin returned content that does not match %s", ErrChunkMissing, hash)
				}
				// Only account for the chunk if it actually reached the disk.
				// Charging curBytes for a failed write filled the budget with
				// entries that could never be read, and eviction then deleted
				// the real chunks to make room for them.
				if werr := writeChunk(s.root, hash, data); werr == nil {
					s.mu.Lock()
					if !s.has(hash) {
						e := &entry{key: hash, size: int64(len(data))}
						el := &element{key: hash}
						s.lru.pushFront(el)
						e.el = el
						s.items[hash] = e
						s.curBytes += int64(len(data))
					}
					s.mu.Unlock()
				}
				return data, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrChunkMissing, hash)
}

// PrefetchHot pulls the hot set into cache in hotness order (background path
// used before resume-from-object-storage). ix.Hot is already ordered
// hottest-first by RecordHotness — the previous explicit sort by chunk index
// discarded that ordering and warmed pages in effectively arbitrary order,
// defeating the hotness index this path exists to exploit.
func (s *Store) PrefetchHot(ctx context.Context, ix *Index) error {
	if s.origin == nil {
		return nil
	}
	for _, i := range ix.Hot {
		if c := ix.Chunks[i]; c.Hash != "" {
			if _, err := s.getChunk(ctx, c.Hash, int64(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordHotness merges observed fault order into the index's hot list.
func (s *Store) RecordHotness(ix *Index, faultOrder []int) error {
	seen := map[int]bool{}
	hot := make([]int, 0, len(ix.Hot)+len(faultOrder))
	for _, i := range faultOrder {
		if i >= 0 && i < len(ix.Chunks) && ix.Chunks[i].Hash != "" && !seen[i] {
			seen[i] = true
			hot = append(hot, i)
		}
	}
	for _, i := range ix.Hot {
		if !seen[i] {
			hot = append(hot, i)
		}
	}
	ix.Hot = hot
	return s.SaveIndex(ix)
}

func (s *Store) SaveIndex(ix *Index) error {
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.indexDir(), ix.ID+".json"), b, 0o644)
}

func (s *Store) LoadIndex(id string) (*Index, error) {
	// Index ids become path components; only platform-generated shapes are
	// honored, so a hostile id can't traverse out of the index dir.
	if len(id) == 0 || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return nil, fmt.Errorf("invalid index id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(s.indexDir(), id+".json"))
	if err != nil {
		return nil, mapOsErr(err)
	}
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, err
	}
	return &ix, nil
}

func mapOsErr(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

var ErrNotFound = errors.New("snapstore: not found")

// Stats exposes cache occupancy for metrics.
func (s *Store) Stats() (chunks int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items), s.curBytes
}
