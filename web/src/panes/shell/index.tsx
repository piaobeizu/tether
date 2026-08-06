import { useEffect, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { useStore } from '../../lib/store'
import { createWT } from '../../lib/wt'
import { activeControlClient } from '../../lib/control'

export default function ShellPane() {
  const containerRef = useRef<HTMLDivElement>(null)
  const { sessionId } = useStore()

  useEffect(() => {
    if (!containerRef.current) return
    const container = containerRef.current

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
        while (true) {
          const { done, value } = await reader.read()
          if (done) break
          term.write(dec.decode(value, { stream: true }))
        }
      }
      pump().catch(() => {})
    }

    connect().catch((err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err)
      term.write(`\r\n[tether] shell connect failed: ${msg}\r\n`)
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
  }, [sessionId])

  return (
    <>
      <div
        ref={containerRef}
        className="pane-body"
        style={{ background: 'var(--bg-sunken)', padding: 0, overflow: 'hidden' }}
      />
    </>
  )
}
