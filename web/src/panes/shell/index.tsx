import { useEffect, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { useStore } from '../../lib/store'
import { createWT } from '../../lib/wt'

export default function ShellPane() {
  const containerRef = useRef<HTMLDivElement>(null)
  const { sessionId } = useStore()

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      fontFamily: '"Cascadia Code", "Fira Code", monospace',
      fontSize: 13,
      theme: { background: '#0a0a0a', foreground: '#cccccc', cursor: '#e8e8e8' },
      cursorBlink: true,
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(containerRef.current)
    fitAddon.fit()

    let wt: WebTransport | null = null
    let writer: WritableStreamDefaultWriter<Uint8Array> | null = null
    let cancelled = false

    const connect = async () => {
      const sid = sessionId ?? ''
      const url = `https://${location.host}/wt/shell${sid ? `?sid=${encodeURIComponent(sid)}` : ''}`
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

    if (sessionId !== null) {
      connect().catch((err: unknown) => {
        const msg = err instanceof Error ? err.message : String(err)
        term.write(`\r\n[tether] shell connect failed: ${msg}\r\n`)
      })
    } else {
      term.write('[tether] waiting for session…\r\n')
    }

    const onResize = () => fitAddon.fit()
    window.addEventListener('resize', onResize)

    return () => {
      cancelled = true
      window.removeEventListener('resize', onResize)
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
