package jsonl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// DefaultSubscriberBuffer is the channel buffer per subscriber. 256
// envelopes ≈ 2-3 turns of cc activity worth of headroom; subscribers
// should drain promptly anyway.
const DefaultSubscriberBuffer = 256

// readChunkSize bounds a single read from the JSONL file. fsnotify
// coalesces WRITE events, so when we wake up there may be hundreds of
// KB to consume; we read in this-sized chunks until EOF.
const readChunkSize = 64 * 1024

// maxUUIDPerSession caps the per-session UUID dedup ring. ~10x typical
// session record count so collisions are vanishingly rare in practice
// while bounding memory at len * (avg uuid len) ≈ 8K * 36 bytes ≈ 288KB
// per active session.
const maxUUIDPerSession = 8192

// Options tunes the Watcher.
type Options struct {
	// SubscriberBuffer is the per-subscriber channel buffer. Zero
	// means DefaultSubscriberBuffer.
	SubscriberBuffer int

	// MapOpts is forwarded to the mapper for every record.
	MapOpts MapOpts

	// OnError, if non-nil, is invoked for parse / IO / fsnotify
	// errors that don't justify killing the watcher. Callers
	// typically log + count these.
	OnError func(path string, err error)

	// OnDrop, if non-nil, is invoked each time an envelope (any
	// class) is dropped because a subscriber's buffer was full.
	//
	// Back-pressure model: the JSONL file ITSELF is the source of
	// truth. The watcher is a derived view; subscribers attaching
	// late or under back-pressure must replay from the file (see
	// SubscribeFromOffset) or rely on the daemon broadcaster's
	// catch-up cache. Blocking this layer would back-pressure the
	// fsnotify drain goroutine and cause the kernel inotify queue to
	// overflow, dropping fs events at a layer where we cannot count
	// or recover them — strictly worse than counted in-flight drops
	// here. Hence: ALL classes use non-blocking drop-newest with
	// EnvelopesDrop counter.
	OnDrop func(sessionID string, kind EnvelopeKind)
}

// Watcher follows a directory of cc JSONL session files. It is the
// owner of all fsnotify state, file offsets, and per-session subscriber
// channels.
//
// Lifecycle:
//
//	w, err := New(ctx, root, Options{})
//	ch := w.Subscribe(sid)
//	for env := range ch { ... }
//	w.Close()        // closes all subscriber channels
//
// Concurrency: New starts one drain goroutine. Subscribe is safe to
// call from any goroutine. Close is idempotent.
type Watcher struct {
	root    string
	dirMode bool // true if root is a directory (vs single-file mode)
	fsw     *fsnotify.Watcher
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	files    map[string]*fileState    // path → state
	subs     map[string][]*subscriber // sessionID → subscribers
	uuidSeen map[string]*uuidRing     // sessionID → bounded FIFO ring (dedup)
	buckets  map[string]struct{}      // bucket dir paths registered with fsnotify
	closed   bool
	wg       sync.WaitGroup
	stats    Stats
}

// uuidRing is a bounded FIFO ring of recently-seen UUIDs. Older entries
// evict on insert once at capacity. Caller must hold the parent lock.
type uuidRing struct {
	capacity int
	seen     map[string]struct{}
	order    []string // FIFO ring
	head     int      // next eviction index
}

func newUUIDRing(capacity int) *uuidRing {
	return &uuidRing{
		capacity: capacity,
		seen:     make(map[string]struct{}, capacity),
		order:    make([]string, 0, capacity),
	}
}

// AddIfNew returns true if the uuid was not previously seen; in that
// case the ring records it (evicting the oldest if at capacity). False
// means duplicate.
func (r *uuidRing) AddIfNew(uuid string) bool {
	if _, dup := r.seen[uuid]; dup {
		return false
	}
	if len(r.order) < r.capacity {
		r.order = append(r.order, uuid)
		r.seen[uuid] = struct{}{}
		return true
	}
	// Evict oldest.
	old := r.order[r.head]
	delete(r.seen, old)
	r.order[r.head] = uuid
	r.head = (r.head + 1) % r.capacity
	r.seen[uuid] = struct{}{}
	return true
}

// Len returns the current number of tracked UUIDs (≤ capacity).
func (r *uuidRing) Len() int { return len(r.seen) }

// Stats are read with atomic loads via the matching getters.
type Stats struct {
	BytesRead     atomic.Int64
	LinesParsed   atomic.Int64
	LinesDropped  atomic.Int64 // failed UTF-8 / decode / too-long
	UUIDDuped     atomic.Int64
	Truncations   atomic.Int64
	Rotations     atomic.Int64
	EnvelopesEmit atomic.Int64
	EnvelopesDrop atomic.Int64
}

// fileState is the per-file watcher record.
type fileState struct {
	path     string
	inode    uint64
	offset   int64
	parser   IncrementalParser
	sid      string // extracted from filename (sid.jsonl)
}

// subscriber is one Subscribe() recipient.
type subscriber struct {
	sid     string
	ch      chan Envelope
	dropped atomic.Int64
}

// New creates a Watcher rooted at `root`.
//
// If `root` is a directory the watcher follows `<root>/*.jsonl` AND
// `<root>/<bucket>/*.jsonl` — exactly one level of subdirectory ("bucket")
// is walked. This matches cc's per-cwd bucket layout
// (`$HOME/.claude/projects/<encoded-cwd>/<sid>.jsonl`, see cc-sdk-route
// §D.1 + §E.5) while remaining bounded — a misconfigured
// `--projects-dir=/` will not walk the whole filesystem. Hidden
// directories (name starts with `.`) are skipped to avoid inotify churn
// from VCS / OS metadata dirs (`.git/`, `.DS_Store/`, ...).
//
// New bucket directories that appear AFTER startup are picked up via
// the Create event on `root` and registered + scanned on the fly.
//
// If `root` is a single .jsonl file the watcher follows just that file
// (single-file mode, behavior unchanged).
//
// New does not block — fsnotify and the drain loop run in goroutines.
// Errors during Setup (root unreadable, fsnotify init fail) return
// immediately; per-file errors during operation are surfaced via
// Options.OnError.
func New(parent context.Context, root string, opts Options) (*Watcher, error) {
	if opts.SubscriberBuffer == 0 {
		opts.SubscriberBuffer = DefaultSubscriberBuffer
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("jsonl watcher: fsnotify: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	w := &Watcher{
		root:     root,
		fsw:      fsw,
		opts:     opts,
		ctx:      ctx,
		cancel:   cancel,
		files:    make(map[string]*fileState),
		subs:     make(map[string][]*subscriber),
		uuidSeen: make(map[string]*uuidRing),
		buckets:  make(map[string]struct{}),
	}

	st, err := os.Stat(root)
	if err != nil {
		fsw.Close()
		cancel()
		return nil, fmt.Errorf("jsonl watcher: stat root: %w", err)
	}

	if st.IsDir() {
		w.dirMode = true
		// Watch root for new bucket dirs + flat .jsonl files.
		if err := fsw.Add(root); err != nil {
			fsw.Close()
			cancel()
			return nil, fmt.Errorf("jsonl watcher: fsnotify add %q: %w", root, err)
		}
		// Pick up flat .jsonl files (legacy single-bucket layout) and
		// register one-level-deep bucket subdirs (cc's actual layout).
		entries, err := os.ReadDir(root)
		if err != nil {
			fsw.Close()
			cancel()
			return nil, fmt.Errorf("jsonl watcher: read root: %w", err)
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue // skip .git, .DS_Store, etc.
			}
			path := filepath.Join(root, name)
			if e.IsDir() {
				if err := w.addBucket(path); err != nil {
					w.reportError(path, err)
				}
				continue
			}
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			if err := w.openFile(path); err != nil {
				w.reportError(path, err)
			}
		}
	} else {
		// Single-file mode. fsnotify needs the *parent* directory
		// to detect rename / replace; we filter to this one path.
		watchTarget := filepath.Dir(root)
		if err := w.openFile(root); err != nil {
			fsw.Close()
			cancel()
			return nil, err
		}
		if err := fsw.Add(watchTarget); err != nil {
			fsw.Close()
			cancel()
			return nil, fmt.Errorf("jsonl watcher: fsnotify add %q: %w", watchTarget, err)
		}
	}

	w.wg.Add(1)
	go w.drain()

	return w, nil
}

// addBucket registers a one-level subdir of root as an additional
// fsnotify watch + scans its existing .jsonl files. Used at startup and
// at runtime when a new bucket Create event fires.
//
// Bounded recursion: we walk subdirs of `root` only. If the bucket
// itself contains nested subdirectories, those are NOT recursed —
// cc-sdk-route §D.1/§E.5 do not promise nested buckets, and unbounded
// recursion would let a misconfigured `--projects-dir=/` hang the
// daemon walking the whole filesystem. Nested dirs are reported via
// OnError and otherwise ignored.
func (w *Watcher) addBucket(path string) error {
	w.mu.Lock()
	if _, exists := w.buckets[path]; exists {
		w.mu.Unlock()
		return nil
	}
	w.buckets[path] = struct{}{}
	w.mu.Unlock()

	if err := w.fsw.Add(path); err != nil {
		// roll back so a future Create can retry
		w.mu.Lock()
		delete(w.buckets, path)
		w.mu.Unlock()
		return fmt.Errorf("jsonl watcher: fsnotify add bucket %q: %w", path, err)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("jsonl watcher: read bucket %q: %w", path, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			// Reject multi-level structure with a clear error so a
			// misconfigured projects-dir surfaces fast instead of
			// silently dropping records.
			w.reportError(filepath.Join(path, name), fmt.Errorf(
				"jsonl watcher: nested directory under bucket not supported (cc layout is exactly one level: root/<bucket>/<sid>.jsonl)",
			))
			continue
		}
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		fp := filepath.Join(path, name)
		if err := w.openFile(fp); err != nil {
			w.reportError(fp, err)
		}
	}
	return nil
}

// dropBucket removes a bucket from tracking when its directory is
// removed/renamed: drops fileState for any files under that bucket and
// forgets the bucket itself. fsnotify automatically removes the watch
// for a deleted directory, so we don't call fsw.Remove here.
func (w *Watcher) dropBucket(path string) {
	prefix := path + string(os.PathSeparator)
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.buckets[path]; !ok {
		return
	}
	delete(w.buckets, path)
	for fp := range w.files {
		if strings.HasPrefix(fp, prefix) {
			delete(w.files, fp)
		}
	}
}

// Subscribe returns a channel that receives Envelopes for the given
// cc session id. Multiple Subscribe calls for the same sid each get
// their own channel (fan-out to multiple consumers). The channel is
// closed by Close().
//
// Equivalent to SubscribeFromOffset(sid, OffsetLiveOnly).
func (w *Watcher) Subscribe(sid string) <-chan Envelope {
	ch, _ := w.SubscribeFromOffset(sid, OffsetLiveOnly)
	return ch
}

// OffsetLiveOnly is the only fromOffset supported in v0.1 — the
// subscriber receives only envelopes the watcher emits AFTER subscribe.
// Pre-existing file content is NOT replayed.
//
// Full file replay (e.g. fromOffset=0) is a follow-up tracked in the
// PR description; the daemon broadcaster's catch-up cache fills the
// gap for v0.1.
const OffsetLiveOnly int64 = -1

// SubscribeFromOffset returns a channel like Subscribe with explicit
// offset semantics. v0.1 only supports OffsetLiveOnly (-1); any other
// value returns an error so callers fail fast instead of silently
// receiving an empty stream.
//
// The full file-replay implementation requires per-subscriber file
// readers + careful interleave with the live drain to avoid races
// between historical parse and live append; tracked as a follow-up.
// Callers that need full replay should:
//  1. Subscribe BEFORE the watcher starts emitting (e.g. immediately
//     after New()), or
//  2. Use the daemon broadcaster's catch-up cache (Epic #3 sub-task —
//     daemon-side LRU per (session, skill)).
func (w *Watcher) SubscribeFromOffset(sid string, fromOffset int64) (<-chan Envelope, error) {
	if fromOffset != OffsetLiveOnly {
		return nil, fmt.Errorf(
			"jsonl watcher: SubscribeFromOffset(fromOffset=%d) not supported in v0.1; pass OffsetLiveOnly (-1) or use the daemon broadcaster's catch-up cache for resume / late-attach",
			fromOffset,
		)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// Closed-watcher contract: hand out a closed channel.
		ch := make(chan Envelope)
		close(ch)
		return ch, nil
	}
	sub := &subscriber{
		sid: sid,
		ch:  make(chan Envelope, w.opts.SubscriberBuffer),
	}
	w.subs[sid] = append(w.subs[sid], sub)
	return sub.ch, nil
}

// Unsubscribe removes the subscriber registered for sid whose channel
// matches ch from the watcher's dispatch list. Idempotent: no-op if
// the watcher is closed (Close already drained everyone) or if (sid,
// ch) doesn't match a live subscriber.
//
// This is the leak-stopper for callers that subscribe and unsubscribe
// repeatedly during the watcher's lifetime (e.g. one Subscribe per
// attach connection on the daemon's Unix socket). Without it, every
// Subscribe call appends to w.subs[sid] indefinitely; this method
// removes the entry so dispatch stops doing wasted work + firing
// OnDrop on a dead consumer, and the channel becomes eligible for GC
// once the caller drops its reference.
//
// Receiver termination contract (important): Unsubscribe DOES NOT
// close the channel. Closing it would race against `deliver()`, which
// snapshots `w.subs[sid]` under mu and then sends non-blockingly
// outside mu — sending to a closed chan panics even inside a select.
// The receiver goroutine must therefore terminate on its OWN signal
// (ctx cancellation, a sibling done channel, etc.), not on a channel-
// close from this side. After Unsubscribe returns, no further
// envelopes will be delivered (subsequent deliver() calls see a fresh
// snapshot without this subscriber); a small number of in-flight
// envelopes already buffered in the channel may still be present
// from a deliver() that copied the slice just before Unsubscribe ran,
// but they will be GC'd when the receiver exits.
//
// Channel-identity match: pass the SAME `<-chan Envelope` value
// Subscribe returned. Equality is the underlying chan-header
// identity; assigning through `chan Envelope` aliases or wrapping in
// a goroutine indirection that hides the identity will not match.
func (w *Watcher) Unsubscribe(sid string, ch <-chan Envelope) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	list := w.subs[sid]
	for i, s := range list {
		// `(<-chan Envelope)(s.ch) == ch` compares the channel header
		// pointer; two channels are equal iff they refer to the same
		// underlying make()'d chan.
		if (<-chan Envelope)(s.ch) == ch {
			w.subs[sid] = append(list[:i], list[i+1:]...)
			if len(w.subs[sid]) == 0 {
				delete(w.subs, sid)
			}
			return
		}
	}
}

// HasSubscribers reports whether at least one live subscriber is
// registered for `sid`. Used by external sweepers that need to know
// "is anyone currently listening to this session?" before deciding
// to delete its on-disk JSONL. Returns false if the watcher is closed.
//
// Concurrency: safe to call from any goroutine. The returned bool is
// a point-in-time read — callers MUST treat it as advisory and combine
// it with their own ordering (e.g. delete only files older than M
// minutes, so a subscriber attaching mid-sweep is racing against an
// already-stale file).
func (w *Watcher) HasSubscribers(sid string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	return len(w.subs[sid]) > 0
}

// Close stops the watcher, closes all subscriber channels, and
// releases fsnotify resources. Idempotent.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	w.cancel()
	err := w.fsw.Close()
	w.wg.Wait()

	w.mu.Lock()
	for _, list := range w.subs {
		for _, s := range list {
			close(s.ch)
		}
	}
	w.subs = nil
	w.mu.Unlock()
	return err
}

// StatsSnapshot returns a copy of the current counters. Field-by-field
// atomic loads — values are point-in-time consistent per field, not
// across the struct.
func (w *Watcher) StatsSnapshot() (out struct {
	BytesRead     int64
	LinesParsed   int64
	LinesDropped  int64
	UUIDDuped     int64
	Truncations   int64
	Rotations     int64
	EnvelopesEmit int64
	EnvelopesDrop int64
}) {
	out.BytesRead = w.stats.BytesRead.Load()
	out.LinesParsed = w.stats.LinesParsed.Load()
	out.LinesDropped = w.stats.LinesDropped.Load()
	out.UUIDDuped = w.stats.UUIDDuped.Load()
	out.Truncations = w.stats.Truncations.Load()
	out.Rotations = w.stats.Rotations.Load()
	out.EnvelopesEmit = w.stats.EnvelopesEmit.Load()
	out.EnvelopesDrop = w.stats.EnvelopesDrop.Load()
	return
}

// ----- internals -----

func (w *Watcher) reportError(path string, err error) {
	if w.opts.OnError != nil {
		w.opts.OnError(path, err)
	}
}

// drain is the fsnotify event loop. Owns all w.files mutations.
func (w *Watcher) drain() {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.reportError("", err)
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	// In dir mode, Create events at the root level may be either a new
	// .jsonl file (legacy flat layout) OR a new bucket subdirectory
	// (cc's actual layout). Rename/Remove on a tracked bucket dir
	// triggers cleanup. We only do these checks in dir mode.
	if w.dirMode {
		// Skip hidden entries (.git, .DS_Store, ...) regardless of op.
		if strings.HasPrefix(filepath.Base(ev.Name), ".") {
			return
		}
		if ev.Op&fsnotify.Create != 0 {
			// stat may race a Remove; ignore ENOENT.
			if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
				// Only register subdirs of root as buckets; nested
				// dirs (would be under an existing bucket) are
				// rejected by addBucket via the OnError path.
				if filepath.Dir(ev.Name) == w.root {
					if err := w.addBucket(ev.Name); err != nil {
						w.reportError(ev.Name, err)
					}
				}
				return
			}
		}
		if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
			// If a tracked bucket dir was removed/renamed, drop its
			// file state. Cheap: dropBucket no-ops when the path
			// isn't a registered bucket.
			w.dropBucket(ev.Name)
		}
	}

	// Filter on .jsonl suffix for the file-level branches.
	if !strings.HasSuffix(ev.Name, ".jsonl") {
		return
	}
	switch {
	case ev.Op&fsnotify.Create != 0:
		// New file — start tracking it.
		if err := w.openFile(ev.Name); err != nil {
			w.reportError(ev.Name, err)
			return
		}
		// CC may write the first line in the same kernel tick as
		// the create; drain immediately.
		w.readFile(ev.Name)
	case ev.Op&fsnotify.Write != 0:
		w.readFile(ev.Name)
	case ev.Op&fsnotify.Rename != 0, ev.Op&fsnotify.Remove != 0:
		// File rotated away. We don't auto-follow rotations
		// across renames — cc doesn't rotate session files in
		// practice. Drop the tracking state.
		w.dropFile(ev.Name)
	}
}

func (w *Watcher) openFile(path string) error {
	w.mu.Lock()
	if _, exists := w.files[path]; exists {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("openFile stat %q: %w", path, err)
	}
	ino, _ := inodeOf(st)
	sid := sidFromPath(path)

	w.mu.Lock()
	w.files[path] = &fileState{
		path:   path,
		inode:  ino,
		offset: 0, // start from the top — full replay on first attach
		sid:    sid,
		parser: IncrementalParser{
			OnError: func(line []byte, err error) {
				w.stats.LinesDropped.Add(1)
				w.reportError(path, fmt.Errorf("parse: %w", err))
			},
		},
	}
	w.mu.Unlock()
	return nil
}

func (w *Watcher) dropFile(path string) {
	w.mu.Lock()
	delete(w.files, path)
	w.mu.Unlock()
}

// readFile picks up new bytes from `path` starting at the recorded
// offset. Detects truncation (file shrank) and rotation (inode
// changed) and resets state accordingly.
func (w *Watcher) readFile(path string) {
	w.mu.Lock()
	fs, ok := w.files[path]
	w.mu.Unlock()
	if !ok {
		// File appeared without our knowing — open it now.
		if err := w.openFile(path); err != nil {
			w.reportError(path, err)
			return
		}
		w.mu.Lock()
		fs = w.files[path]
		w.mu.Unlock()
	}

	st, err := os.Stat(path)
	if err != nil {
		// Probably racing a delete; ignore.
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		w.reportError(path, err)
		return
	}

	curIno, _ := inodeOf(st)
	curSize := st.Size()

	// Rotation: inode changed. Reset offset + parser.
	if fs.inode != 0 && curIno != 0 && curIno != fs.inode {
		w.stats.Rotations.Add(1)
		fs.inode = curIno
		fs.offset = 0
		fs.parser.Reset()
	}

	// Truncation: file shrank. Reset offset.
	if curSize < fs.offset {
		w.stats.Truncations.Add(1)
		fs.offset = 0
		fs.parser.Reset()
	}

	if curSize == fs.offset {
		return // nothing new
	}

	f, err := os.Open(path)
	if err != nil {
		w.reportError(path, err)
		return
	}
	defer f.Close()

	if _, err := f.Seek(fs.offset, io.SeekStart); err != nil {
		w.reportError(path, err)
		return
	}

	buf := make([]byte, readChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			w.stats.BytesRead.Add(int64(n))
			fs.offset += int64(n)
			records := fs.parser.Feed(buf[:n])
			for _, rec := range records {
				w.dispatch(fs, rec)
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				w.reportError(path, rerr)
			}
			break
		}
	}
}

// dispatch classifies + maps + delivers a record to all subscribers
// of its session id.
func (w *Watcher) dispatch(fs *fileState, rec Record) {
	w.stats.LinesParsed.Add(1)

	// session id: prefer record's sessionId, fall back to filename.
	sid := rec.SessionID
	if sid == "" {
		sid = fs.sid
	}

	// UUID dedup (per session, bounded FIFO ring).
	if rec.UUID != "" {
		w.mu.Lock()
		ring, ok := w.uuidSeen[sid]
		if !ok {
			ring = newUUIDRing(maxUUIDPerSession)
			w.uuidSeen[sid] = ring
		}
		isNew := ring.AddIfNew(rec.UUID)
		w.mu.Unlock()
		if !isNew {
			w.stats.UUIDDuped.Add(1)
			return
		}
	}

	class := Classify(rec)
	env, ok := Map(rec, class, w.opts.MapOpts)
	if !ok {
		return
	}
	if env.SessionID == "" {
		env.SessionID = sid
	}

	w.deliver(env)
}

// deliver hands the envelope to every subscriber of env.SessionID.
//
// Drop policy: ALL classes use non-blocking drop-newest with the
// EnvelopesDrop counter. Rationale (see Options.OnDrop docstring):
// blocking would back-pressure the fsnotify drain goroutine into
// dropping kernel-level inotify events, which we cannot count or
// recover. The JSONL file is the source of truth; the daemon
// broadcaster's catch-up cache (and a future SubscribeFromOffset
// replay) handles late-attach / lossy delivery, NOT this layer.
//
// "Stream 2 永不丢" (§3.3.3) is the daemon broadcaster's contract,
// not the watcher's — this layer is a derived view.
func (w *Watcher) deliver(env Envelope) {
	w.mu.Lock()
	subs := append([]*subscriber(nil), w.subs[env.SessionID]...)
	w.mu.Unlock()

	w.stats.EnvelopesEmit.Add(1)

	for _, s := range subs {
		select {
		case s.ch <- env:
		default:
			s.dropped.Add(1)
			w.stats.EnvelopesDrop.Add(1)
			if w.opts.OnDrop != nil {
				w.opts.OnDrop(env.SessionID, env.Kind)
			}
		}
	}
}

// inodeOf returns the underlying inode number on platforms where it
// exists (Linux, macOS). Falls back to 0 (which the caller treats as
// "rotation detection unavailable").
func inodeOf(st os.FileInfo) (uint64, bool) {
	sys := st.Sys()
	if sys == nil {
		return 0, false
	}
	if s, ok := sys.(*syscall.Stat_t); ok {
		return uint64(s.Ino), true
	}
	return 0, false
}

// sidFromPath extracts the session UUID from `<root>/<sid>.jsonl`.
// Returns the basename without `.jsonl` — even if it isn't a valid
// UUID, the daemon's higher layers map session-by-string anyway.
func sidFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".jsonl")
}
