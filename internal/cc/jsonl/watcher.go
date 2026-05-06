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

	// OnDrop, if non-nil, is invoked each time a STATE/HOOK
	// envelope is dropped because a subscriber's buffer was full.
	// EVENT envelopes are never silently dropped — they block the
	// per-file reader (see watcherSubscriber.deliver).
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
	root  string
	fsw   *fsnotify.Watcher
	opts  Options
	ctx   context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	files      map[string]*fileState     // path → state
	subs       map[string][]*subscriber  // sessionID → subscribers
	uuidSeen   map[string]map[string]struct{} // sessionID → uuid set (dedup)
	closed     bool
	wg         sync.WaitGroup
	stats      Stats
}

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

// New creates a Watcher rooted at `root`. If `root` is a directory
// the watcher follows `<root>/*.jsonl` (and any new files matching
// that pattern that appear). If `root` is a single .jsonl file the
// watcher follows just that file.
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
		uuidSeen: make(map[string]map[string]struct{}),
	}

	st, err := os.Stat(root)
	if err != nil {
		fsw.Close()
		cancel()
		return nil, fmt.Errorf("jsonl watcher: stat root: %w", err)
	}

	var watchTarget string
	if st.IsDir() {
		watchTarget = root
		// Pick up files already present.
		entries, err := os.ReadDir(root)
		if err != nil {
			fsw.Close()
			cancel()
			return nil, fmt.Errorf("jsonl watcher: read root: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(root, e.Name())
			if err := w.openFile(path); err != nil {
				w.reportError(path, err)
			}
		}
	} else {
		// Single-file mode. fsnotify needs the *parent* directory
		// to detect rename / replace; we filter to this one path.
		watchTarget = filepath.Dir(root)
		if err := w.openFile(root); err != nil {
			fsw.Close()
			cancel()
			return nil, err
		}
	}

	if err := fsw.Add(watchTarget); err != nil {
		fsw.Close()
		cancel()
		return nil, fmt.Errorf("jsonl watcher: fsnotify add %q: %w", watchTarget, err)
	}

	w.wg.Add(1)
	go w.drain()

	return w, nil
}

// Subscribe returns a channel that receives Envelopes for the given
// cc session id. Multiple Subscribe calls for the same sid each get
// their own channel (fan-out to multiple consumers). The channel is
// closed by Close().
func (w *Watcher) Subscribe(sid string) <-chan Envelope {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// Closed-watcher contract: hand out a closed channel.
		ch := make(chan Envelope)
		close(ch)
		return ch
	}
	sub := &subscriber{
		sid: sid,
		ch:  make(chan Envelope, w.opts.SubscriberBuffer),
	}
	w.subs[sid] = append(w.subs[sid], sub)
	return sub.ch
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
	// Filter on .jsonl suffix early.
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

	// UUID dedup (per session).
	if rec.UUID != "" {
		w.mu.Lock()
		seen, ok := w.uuidSeen[sid]
		if !ok {
			seen = make(map[string]struct{})
			w.uuidSeen[sid] = seen
		}
		if _, dup := seen[rec.UUID]; dup {
			w.stats.UUIDDuped.Add(1)
			w.mu.Unlock()
			return
		}
		seen[rec.UUID] = struct{}{}
		w.mu.Unlock()
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
// Drop policy:
//   - EVENT class: never drop. Block the per-file reader until at
//     least one subscriber has buffer room. (Stream 2 永不丢, §3.3.3.)
//   - HOOK / STATE class: best-effort non-blocking send. On full
//     buffer, drop-newest and increment counters.
func (w *Watcher) deliver(env Envelope) {
	w.mu.Lock()
	subs := append([]*subscriber(nil), w.subs[env.SessionID]...)
	w.mu.Unlock()

	w.stats.EnvelopesEmit.Add(1)

	for _, s := range subs {
		switch env.Class {
		case ClassEvent:
			// Blocking send (with ctx cancellation).
			select {
			case s.ch <- env:
			case <-w.ctx.Done():
				return
			}
		default:
			// Non-blocking, drop-newest.
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
