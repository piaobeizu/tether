// Package jsonl implements the cc JSONL session-file watcher, incremental
// parser, three-class classifier, and JSONL-record-to-wire-envelope mapper
// for the ClaudeCodeProvider.
//
// # What this package owns
//
// cc writes its session history as line-delimited JSON to
//
//	~/.claude/projects/<workspace-bucket>/<sid>.jsonl
//
// where <workspace-bucket> is the cwd-encoded path (see internal/cc.path),
// and <sid> is the cc session UUID. Every cc subprocess append-writes a
// new JSON record per turn / per tool call / per state mutation. tether
// MUST treat that file as read-only (E.1 contract; see contracts_test in
// internal/backend/claude). Our pipeline:
//
//  1. Watcher (watcher.go) — fsnotify-based per-file follower. Tracks
//     (path → fileState{inode, offset, partialBuf}). On WRITE events it
//     reads from the recorded offset to EOF, hands the bytes to the
//     incremental Parser, and re-arms the offset. Detects truncation
//     (size shrank), rotation (inode changed), and rename / remove.
//     Subscribers receive Envelopes for one specific session id.
//
//  2. Parser (parser.go) — line-oriented incremental decoder. Holds a
//     partial-line buffer across reads (cc may flush a record halfway
//     through; F-09 torn-write defense: only lines terminated by '\n'
//     are emitted). Strict UTF-8: invalid sequences are reported via
//     OnError and the offending line is dropped, not silently mangled.
//     Multi-line records are NOT a thing in cc JSONL — every record is
//     exactly one line, terminated by exactly one '\n'.
//
//  3. Classifier (classifier.go) — three-class taxonomy per spec §5.6
//     "三类分类 (F-04)". See the table below — that table is the
//     authoritative reference for every other module that consumes
//     these records.
//
//  4. Mapper (mapper.go) — per spec §11.N, transforms an EVENT-class
//     record into a wire envelope (Envelope struct in this package;
//     downstream encryption is left to Epic #5 — the Mapper produces
//     a CiphertextPayload []byte that is the *plaintext* JSON-encoded
//     payload, with a documented seam where the encryption hook will
//     wrap it in v0.1's XChaCha20-Poly1305 layer). HOOK-class records
//     are surfaced as a separate envelope kind so the daemon hook
//     state-machine can react; STATE-class records produce a session
//     state-update envelope that does NOT broadcast on the events
//     stream (consumers route it to the local session view).
//
// # F-04 / F-09 classification table (authoritative)
//
//	┌────────┬──────────────────────────────────────┬──────────────────────────────────┐
//	│ Class  │ JSONL record `type` values           │ Routing                          │
//	├────────┼──────────────────────────────────────┼──────────────────────────────────┤
//	│ EVENT  │ "user", "assistant"                  │ wire `output.agent-event`        │
//	│        │   (have `uuid`, parent chain)        │   broadcast to mobile/attach     │
//	├────────┼──────────────────────────────────────┼──────────────────────────────────┤
//	│ HOOK   │ "attachment" with non-empty          │ wire `output.hook-event`         │
//	│        │   `attachment.hookEvent`             │   updates daemon hook FSM        │
//	│        │   (e.g. SessionStart, PreToolUse)    │                                  │
//	├────────┼──────────────────────────────────────┼──────────────────────────────────┤
//	│ STATE  │ "permission-mode", "last-prompt",    │ session.state envelope           │
//	│        │   "file-history-snapshot",           │   (NOT broadcast on events       │
//	│        │   "ai-title", "system",              │    stream; updates local view)   │
//	│        │   "attachment" with EMPTY            │                                  │
//	│        │   `attachment.hookEvent`             │                                  │
//	└────────┴──────────────────────────────────────┴──────────────────────────────────┘
//
// Records with an unrecognized type are routed to STATE (defensive
// default) and reported via the watcher's metrics counter; we do NOT
// drop them — cc may add new record types in patch versions and the
// daemon must stay forward-compatible.
//
// # Torn-write defense (F-09)
//
// PoC-1 step 9 measured 0/45000 torn-write hits at 1ms polling. We still
// hold the line: only records ending in '\n' are emitted; everything
// after the last '\n' is held in the partial buffer until the next read
// completes the record. UUID-level dedup (a per-session sync.Map of
// uuids we've already emitted) catches the rare case where cc retries
// a write and lands the same uuid twice.
//
// # Back-pressure policy
//
// Subscribe() returns a buffered channel (default 256 envelopes) per
// subscriber. When the buffer fills the watcher does NOT block on the
// reader (that would propagate back to the fsnotify drain loop and
// eventually drop kernel-level events). Instead the watcher uses a
// non-blocking send and increments a per-subscriber "dropped" counter.
// EVENT-class envelopes are never dropped on the path-watcher side
// because the daemon broadcaster reads them synchronously and Stream 2
// (events) is "永不丢" per spec §3.3.3 — drops here would be a contract
// violation. STATE/HOOK envelopes are best-effort; subscribers that
// care must drain promptly. The drop policy is "drop-newest" (preserve
// causal order of what subscribers do see). See watcherSubscriber.
//
// # Fence-tag suffix grep (sub-task #11 hook)
//
// Spec §11.AA.1 locks the fenced-block protocol with the `<type>:<skill>`
// suffix syntax (e.g. ```dag:writing-plans). The grep that detects fence
// lines lives in mapper.go behind an explicit, named seam:
// extractFenceTag(line string) (blockType, skillName string, ok bool)
// is unexported but documented as the integration point for sub-task
// #11. Today it returns ok=false unconditionally — the mapper never
// peers inside content blocks (D-5: daemon stays transparent). When
// sub-task #11 turns this on, the mapper attaches blockType / skillName
// to envelope.PlaintextMetadata and the daemon catch-up LRU keys on
// (sessionId, skillName, blockId). Adding fence-tag awareness here is
// strictly additive — the wire schema already has the metadata slots.
//
// # Encryption seam (Epic #5)
//
// Mapper.Map produces an Envelope whose CiphertextPayload field holds
// the *plaintext* JSON encoding of the payload. The naming is
// deliberately forward-looking: a single-line wrap in pipeline.go
// (`env.CiphertextPayload = encrypt(env.SessionID, env.CiphertextPayload)`)
// flips this layer to real ciphertext. Until Epic #5 ships, downstream
// consumers MUST treat the payload as already-decrypted plaintext.
package jsonl
