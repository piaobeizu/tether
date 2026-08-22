import { useCallback, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { useStore } from '../../lib/store'
import { createWT } from '../../lib/wt'
import { activeControlClient } from '../../lib/control'

// ── What this pane can, and cannot, tell the user about an ending (tether#151) ─
//
// Until tether#151 both of the ways a shell ends were handled by saying nothing.
// The read loop `break`ed on `done` and `pump().catch(() => {})` swallowed the
// other half, so a user whose `exit` had just been executed and a user whose
// laptop had just changed networks saw the same thing: a terminal that had
// stopped responding, with no explanation on screen and nothing to click. The
// pane stayed like that until the page was reloaded.
//
// Those two SHOULD read differently. "your shell finished" is not "the link
// dropped", and only the second is even a candidate for reconnecting — silently
// reconnecting after a clean `exit` would spawn a second shell nobody asked for.
// The problem is that the browser is not told which one happened, and that is
// not something this file can work around:
//
//   - The daemon does encode the distinction, in the WebTransport close code.
//     internal/server/wt_shell.go's handleWTShell hands pumpShell a closer that
//     is CloseWithError(0, ""), and uses CloseWithError(1, "pty start failed")
//     for a PTY that never started.
//   - The code does not arrive. The doc comment on refusalDrainGrace in
//     internal/server/wt_chat.go records the measurement (Chrome, headless,
//     against this daemon): Chrome rejected `WebTransport.closed` with
//     "Connection lost" rather than resolving it with the code and reason, and
//     it did so for a custom code as well as for 0 — so nothing the daemon puts
//     there is readable by the browser at all. The chat route works around that
//     by sending its refusal as an in-band envelope and holding the session open
//     long enough to deliver it. /wt/shell has no envelope channel — it is raw
//     PTY bytes by contract (D-05a §2 fact 4) — and the one in-band marker it
//     does write covers a different event, the PTY failing to START.
//
// So the distinction is genuinely not available here, and inventing a heuristic
// for it would be worse than admitting it: a guess of "the link dropped" relaunches
// a shell every time somebody types `exit`. What follows therefore reports the one
// fact it can stand behind — this pane is no longer attached to a shell — and
// leaves the decision to start another one with the user, who does know which of
// the two just happened. Nothing here reconnects on its own, on any path.
//
// Giving /wt/shell a signal a client could act on automatically is a daemon-side
// change and is tracked separately.

/**
 * What the pane says when the shell stream is over, whichever half ended first.
 *
 * Deliberately does not claim the shell exited OR that the connection dropped:
 * see the block above for why neither is knowable from here, and note that a
 * wrong claim here is not cosmetic — it is the sentence a user decides whether
 * to reopen a shell on.
 */
const SHELL_OVER = {
  headline: 'Shell disconnected',
  detail:
    'The shell stream ended — the shell may have exited, or the connection may have dropped. ' +
    'tether cannot tell which, so it will not start a new one for you.',
}

/** Headline for a connection that never came up at all. The detail is the error. */
const SHELL_CONNECT_FAILED = 'Shell connection failed'

/** The one control on the banner. A CLICK, never a timer — see the block above. */
const SHELL_RESTART_LABEL = 'Start a new shell'

/** A finished connection, as shown to the user. `null` while one is live. */
type ShellEnd = { headline: string; detail: string }

export default function ShellPane() {
  const containerRef = useRef<HTMLDivElement>(null)
  const { sessionId } = useStore()

  // The banner. Not derived from anything: it is set once per connection, by
  // whichever of the three end paths below gets there first, and cleared only by
  // a new connection starting.
  const [ended, setEnded] = useState<ShellEnd | null>(null)

  // Bumping this is the ONLY thing that re-runs the effect without the session
  // changing, and the only thing that bumps it is the button's onClick. That is
  // what makes "this pane never reconnects by itself" a property of the deps
  // array rather than of a promise never being resolved somewhere.
  const [attempt, setAttempt] = useState(0)
  const restart = useCallback(() => {
    setEnded(null)
    setAttempt((n) => n + 1)
  }, [])

  useEffect(() => {
    if (!containerRef.current) return
    const container = containerRef.current

    // A new connection starts with a clean slate. Reached on a session switch as
    // well as on a restart click; setting null when it is already null is a
    // no-op re-render-wise, so the ordinary mount pays nothing for it.
    setEnded(null)

    const term = new Terminal({
      fontFamily: '"Cascadia Code", "Fira Code", monospace',
      fontSize: 13,
      theme: { background: '#0a0a0a', foreground: '#cccccc', cursor: '#e8e8e8' },
      cursorBlink: true,
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(container)

    // fit() throws when the element has no layout yet (xterm divides by a cell
    // size it measures as zero). Not worth failing the pane over: the observer
    // below fires again as soon as the element does have a size.
    const refit = () => {
      try {
        fitAddon.fit()
      } catch {
        // not laid out yet
      }
    }
    refit()

    const sid = sessionId ?? ''

    // Report every size xterm settles on so the daemon can retarget the PTY
    // (tether#68). Before this, fit() resized only the browser's view while the
    // remote TUI kept rendering for the size the PTY was started at — which is
    // what produced the clipped, overlapping Shell output.
    //
    // No debounce: xterm fires onResize only when the computed cols/rows
    // actually change, so dragging the divider across many pixels still emits
    // at most one frame per column crossed.
    term.onResize(({ cols, rows }) => {
      void activeControlClient()?.sendResize(sid, cols, rows)
    })

    let wt: WebTransport | null = null
    let writer: WritableStreamDefaultWriter<Uint8Array> | null = null
    let cancelled = false

    // settle, once, from whichever end path arrives first.
    //
    // There are three, and at least two of them fire on an ordinary shell exit:
    // the daemon closes the WebTransport session and closes the stream, so the
    // reader and `wt.closed` both settle and their order is not fixed. Guarded
    // rather than de-duplicated at the source because the alternative — picking
    // one path and trusting it — is exactly the assumption that leaves a pane
    // silent when the other one happens to be the one that fires.
    //
    // `cancelled` (unmount, session switch) is not an ending the user needs told
    // about: the pane is going away or is about to be rebuilt.
    let settled = false
    const finish = (end: ShellEnd) => {
      if (cancelled || settled) return
      settled = true
      setEnded(end)
    }

    const connect = async () => {
      const q = new URLSearchParams()
      if (sid) q.set('sid', sid)
      // Start the PTY at the size we are already displaying. Relying on the
      // first resize frame instead would paint one screenful at the kernel
      // default and then reflow it, and would leave the size wrong for the
      // whole session if the control lane never connects.
      if (term.cols > 0 && term.rows > 0) {
        q.set('cols', String(term.cols))
        q.set('rows', String(term.rows))
      }
      const query = q.toString()
      const url = `https://${location.host}/wt/shell${query ? `?${query}` : ''}`
      wt = await createWT(url)

      const bidi = await wt.createBidirectionalStream()
      writer = bidi.writable.getWriter()

      // Watched only from here on, i.e. only once there is a shell to lose. A
      // session that dies during the handshake above makes createBidirectionalStream
      // throw, and that is a failure to connect, not a shell that ended.
      //
      // Both settle paths mean the same thing. `closed` REJECTS rather than
      // resolves when the session went away abruptly, and lib/wt.ts's tether#63
      // note records that Chrome takes the rejecting path even for a graceful
      // daemon-side close — so treating a rejection as anything other than an
      // ending is how a pane ends up sitting on a dead transport.
      void wt.closed.then(() => finish(SHELL_OVER)).catch(() => finish(SHELL_OVER))

      // Terminal input → WT stream.
      term.onData((data) => {
        if (cancelled) return
        const bytes = new TextEncoder().encode(data)
        writer?.write(bytes).catch(() => {})
      })

      // WT stream → terminal (raw PTY bytes, no framing per D-05a §2 fact 4).
      const reader = bidi.readable.getReader()
      const pump = async () => {
        const dec = new TextDecoder()
        try {
          while (true) {
            const { done, value } = await reader.read()
            if (done) break
            term.write(dec.decode(value, { stream: true }))
          }
        } catch {
          // A reset stream and a clean FIN carry the same news here: there is
          // nothing more coming. Swallowing the error is fine; swallowing the
          // FACT of it, which is what `pump().catch(() => {})` used to do, is
          // the bug — the `.then` below runs on both.
        }
      }
      void pump().then(() => finish(SHELL_OVER))
    }

    connect().catch((err: unknown) => {
      // Was written into the terminal until tether#151. Moved into the banner
      // because a message in the terminal is a message the user cannot act on:
      // the same string now arrives next to the button that does something
      // about it, and is somewhere a test can read it.
      finish({ headline: SHELL_CONNECT_FAILED, detail: err instanceof Error ? err.message : String(err) })
    })

    // Observe the CONTAINER, not the window. This pane's width also changes
    // when the column divider is dragged, when switching to this tab reveals
    // it, and when the left sidebar collapses — none of which fire a window
    // resize, so the old window-only listener missed every one of them.
    const observer = new ResizeObserver(refit)
    observer.observe(container)

    return () => {
      cancelled = true
      observer.disconnect()
      writer?.close().catch(() => {})
      wt?.close()
      term.dispose()
    }
  }, [sessionId, attempt])

  return (
    <>
      <div
        ref={containerRef}
        className="pane-body"
        style={{ background: 'var(--bg-sunken)', padding: 0, overflow: 'hidden' }}
      />
      {ended && (
        <div
          data-testid="shell-status"
          role="status"
          style={{
            display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0,
            padding: '8px 12px',
            borderTop: '1px solid var(--line-soft)',
            background: 'var(--bg-tint)',
            fontSize: 12, color: 'var(--ink-secondary)',
          }}
        >
          <div style={{ minWidth: 0 }}>
            <div style={{ fontWeight: 600, color: 'var(--ink-primary)', marginBottom: 2 }}>
              {ended.headline}
            </div>
            <div style={{ fontSize: 11.5, lineHeight: 1.45 }}>{ended.detail}</div>
          </div>
          <button
            type="button"
            className="btn-ghost-sm"
            onClick={restart}
            style={{ marginLeft: 'auto', flexShrink: 0 }}
          >
            {SHELL_RESTART_LABEL}
          </button>
        </div>
      )}
    </>
  )
}
