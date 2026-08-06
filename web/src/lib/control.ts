// control.ts — /wt/control client: periodic ping/pong RTT measurement.
// Started after the main WT connection (chat) is live; feeds smoothed
// latency samples into the app store for display in the titlebar/Settings.
import type { ClientFrame, ControlFrame } from './wire.gen'
import { ClientFramePing, ClientFrameResize, ControlPong } from './wire.gen'
import { createWT } from './wt'
import { applyLatencySample } from './latency'
import { useStore } from './store'

const PING_INTERVAL_MS = 5000
const RECONNECT_DELAY_MS = 5000

/**
 * The ControlClient currently owning the /wt/control lane, if any.
 *
 * ChatPane constructs and owns it (it must not start before the chat
 * connection is live), but ShellPane needs the same lane to report terminal
 * size — and opening a second /wt/control session just for that would mean a
 * second WT session and a second ping loop measuring the same RTT. The lane is
 * a singleton in practice, so it is published here rather than threaded
 * through props/context across two unrelated panes. tether#68.
 */
let active: ControlClient | null = null

/** activeControlClient returns the live control lane, or null if none. */
export function activeControlClient(): ControlClient | null {
  return active
}

/**
 * ControlClient owns a /wt/control WebTransport session: it opens a bidi
 * stream, sends a ping every PING_INTERVAL_MS, and on each pong updates
 * useStore's connection.latency via an EWMA smoothing pass. It reconnects
 * on transport loss (best-effort, independent of the chat lane) until
 * stop() is called.
 */
export class ControlClient {
  private wt: WebTransport | null = null
  private writer: WritableStreamDefaultWriter<Uint8Array> | null = null
  private timer: ReturnType<typeof setInterval> | null = null
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private stopped = false

  /** start (re)enables the control lane and connects to /wt/control. */
  async start(): Promise<void> {
    this.stopped = false
    active = this
    await this.connect()
  }

  private async connect(): Promise<void> {
    if (this.stopped) return
    try {
      const url = `https://${location.host}/wt/control`
      this.wt = await createWT(url)
      const stream = await this.wt.createBidirectionalStream()
      this.writer = stream.writable.getWriter()
      void this.readPongs(stream.readable)
      this.timer = setInterval(() => { void this.sendPing() }, PING_INTERVAL_MS)
      void this.sendPing()
      // On transport close, retry unless the caller stopped us.
      this.wt.closed.catch(() => {}).finally(() => this.onTransportClosed())
    } catch {
      // connect failed — schedule a retry (best-effort; never breaks chat).
      this.onTransportClosed()
    }
  }

  /** Tear down the current transport and, unless stopped, schedule a reconnect. */
  private onTransportClosed(): void {
    if (this.timer !== null) { clearInterval(this.timer); this.timer = null }
    try { this.writer?.releaseLock() } catch { /* already released */ }
    this.writer = null
    this.wt = null
    if (this.stopped) return
    if (this.reconnectTimer === null) {
      this.reconnectTimer = setTimeout(() => {
        this.reconnectTimer = null
        void this.connect()
      }, RECONNECT_DELAY_MS)
    }
  }

  /** stop permanently disables the control lane (no further reconnects). */
  stop(): void {
    this.stopped = true
    if (active === this) active = null
    if (this.timer !== null) { clearInterval(this.timer); this.timer = null }
    if (this.reconnectTimer !== null) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null }
    try { this.writer?.releaseLock() } catch { /* already released */ }
    this.writer = null
    this.wt?.close({ closeCode: 0, reason: 'client close' })
    this.wt = null
  }

  private async sendPing(): Promise<void> {
    if (!this.writer) return
    // ts MUST be an integer: wire.ClientFrame.TS is int64 on the Go side, and
    // json.Unmarshal rejects a fractional number into int64 (dropping the ping).
    // performance.now() is a fractional DOMHighResTimeStamp, so round it.
    const frame: ClientFrame = { kind: ClientFramePing, ts: Math.round(performance.now()) }
    const line = JSON.stringify(frame) + '\n'
    try {
      await this.writer.write(new TextEncoder().encode(line))
    } catch {
      // write failure — stream likely closed; wt.closed will drive teardown.
    }
  }

  /**
   * sendAction writes an "action" ClientFrame on the control stream (D-19
   * §5, tether#8 T8) — e.g. the DAG card's approve button. Best-effort, like
   * sendPing: silently drops if the control stream isn't currently
   * connected, and there's no ack to await.
   */
  async sendAction(frame: ClientFrame): Promise<void> {
    await this.writeFrame(frame)
  }

  /**
   * sendResize tells the daemon the size ShellPane is actually rendering the
   * terminal at, so it can retarget that session's PTY (tether#68).
   *
   * It rides the control lane because /wt/shell carries raw PTY bytes and has
   * nowhere to put a size. Best-effort like the rest of this class: if the
   * control lane is down the frame is dropped, and the PTY keeps the size it
   * was started with (ShellPane also passes the initial size on the /wt/shell
   * query string, so a dropped frame degrades rather than breaks).
   *
   * A zero dimension is never sent — xterm reports 0 while the pane is
   * display:none, and a 0-wide PTY blanks the remote TUI.
   */
  async sendResize(sessionId: string, cols: number, rows: number): Promise<void> {
    if (cols <= 0 || rows <= 0) return
    await this.writeFrame({ kind: ClientFrameResize, sessionId, cols, rows })
  }

  private async writeFrame(frame: ClientFrame): Promise<void> {
    if (!this.writer) return
    const line = JSON.stringify(frame) + '\n'
    try {
      await this.writer.write(new TextEncoder().encode(line))
    } catch {
      // write failure — stream likely closed.
    }
  }

  private async readPongs(readable: ReadableStream<Uint8Array>): Promise<void> {
    const reader = readable.getReader()
    let buf = ''
    try {
      while (true) {
        const { value, done } = await reader.read()
        if (done) break
        buf += new TextDecoder().decode(value)
        let idx: number
        while ((idx = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, idx).trim()
          buf = buf.slice(idx + 1)
          if (!line) continue
          this.handleLine(line)
        }
      }
    } catch {
      // transport closed
    }
  }

  private handleLine(line: string): void {
    if (this.stopped) return
    let frame: ControlFrame
    try {
      frame = JSON.parse(line) as ControlFrame
    } catch {
      return
    }
    if (frame.kind !== ControlPong || typeof frame.ts !== 'number') return
    const rtt = performance.now() - frame.ts
    const prev = useStore.getState().connection.latency
    const next = applyLatencySample(prev, rtt)
    useStore.getState().setConnection({ latency: Math.round(next) })
  }
}
