// wt.ts — WebTransport abstraction with serverCertificateHashes pinning.
// Sequence: fetch /cert-hash → construct WebTransport with hash → open streams.
// On prod (CA-signed cert), skip hash fetch and use normal TLS verification.
import type { Envelope, HashHex64 } from './wire.gen'

export type { Envelope }

export interface WTOptions {
  url: string
  onEnvelope?: (env: Envelope) => void
  onClose?: (code: number, reason: string) => void
}

// KNOWN COVERAGE GAP (tether#63, stated in the same spirit as
// internal/server/wt_chat.go's note on admitChat): nothing in the unit suite
// exercises this class. It needs a real WebTransport, which jsdom does not have
// and which no harness here stands up, so `pnpm test` stays green if the close
// handling below is deleted. It is covered only by live_verify — and it IS
// load-bearing: with the pre-tether#63 `.catch(() => {})` restored, a
// daemon-side refusal leaves the pane sitting on a dead transport showing no
// banner, no card and no retry (measured). Treat edits here as behavioural
// changes that need a live run, not as refactors.
export class TetherWT {
  private wt: WebTransport | null = null
  private opts: WTOptions
  private closed = false
  private closeFired = false

  constructor(opts: WTOptions) {
    this.opts = opts
  }

  async connect(): Promise<void> {
    const [certHash, ticket] = await Promise.all([fetchCertHash(), fetchWtTicket()])
    // close() can only close what is already in this.wt, and nothing writes that
    // field until the constructor below — so a close() landing in those two HTTP
    // round trips closed nothing, and this method went on to open a session no
    // caller had a handle to (tether#128). `closed` only made it QUIET: readStream
    // drops its envelopes and fireClose returns early, so the transport stayed up
    // and so did the daemon-side attach behind it. Two real triggers: StrictMode
    // runs an effect, its cleanup, then the effect again, so every dev load put a
    // close() in this window; and ChatPane closes the outgoing TetherWT on retry
    // and reconnect without knowing whether it ever finished connecting.
    //
    // Returning HERE, above the constructor, rather than tearing down afterwards:
    // there is then nothing built to remember to tear down. From the constructor
    // on, this.wt is set before every await, so a close() in the `ready` window
    // below already reaches the real session and needs no second check.
    //
    // Per the notice above this is a behavioural change with no unit coverage and
    // no live run behind it, which is why it is one guard and no teardown. What a
    // live run would confirm: the caller's `.then` still runs on this early
    // return and openBidiStream() throws 'not connected' — the error path a
    // connect that never connected has always taken — so a superseded attempt now
    // reports a failure where it used to report a bogus success.
    if (this.closed) return
    const wtOpts: WebTransportOptions = certHash
      ? { serverCertificateHashes: [{ algorithm: 'sha-256', value: hexToBuffer(certHash) }] }
      : {}
    this.wt = new WebTransport(appendTicket(this.opts.url, ticket), wtOpts)
    await this.wt.ready
    this.readEvents()
    // BOTH settle paths are a close (tether#63). `closed` REJECTS when the
    // session went away abruptly rather than through a graceful
    // CLOSE_WEBTRANSPORT_SESSION, and measurement says that is the ordinary
    // case here, not the exotic one: a daemon-side refusal produced a
    // rejection in 5 of 6 live runs ("WebTransportError: Connection lost").
    // The previous `.catch(() => {})` swallowed exactly those, so the pane's
    // onClose — which owns the reconnect decision — was never told the
    // connection had ended. It got away with that only because the death used
    // to be instant, so `openBidiStream()` threw inside connect()'s own
    // promise chain and THAT reported the failure instead. Once the daemon
    // holds a refused session open long enough to deliver its reason
    // (refusalDrainGrace, wt_chat.go), the bidi stream opens fine and this is
    // the only signal left — a swallowed rejection would leave the UI sitting
    // on a dead transport believing it was connected.
    this.wt.closed
      .then((info) => this.fireClose(info.closeCode ?? 0, info.reason ?? ''))
      .catch((err: unknown) => this.fireClose(0, err instanceof Error ? err.message : String(err)))
  }

  // fireClose reports the close to the caller at most once. Guarded because the
  // two handlers above are mutually exclusive today but need not stay that way,
  // and a doubled onClose would schedule two reconnect ladders.
  private fireClose(code: number, reason: string): void {
    if (this.closed || this.closeFired) return
    this.closeFired = true
    this.opts.onClose?.(code, reason)
  }

  private async readEvents(): Promise<void> {
    if (!this.wt || !this.opts.onEnvelope) return
    const reader = this.wt.incomingUnidirectionalStreams.getReader()
    try {
      while (true) {
        const { value: stream, done } = await reader.read()
        if (done) break
        this.readStream(stream)
      }
    } catch { /* closed */ }
  }

  private async readStream(stream: ReadableStream<Uint8Array>): Promise<void> {
    const reader = stream.getReader()
    const chunks: Uint8Array[] = []
    try {
      while (true) {
        const { value, done } = await reader.read()
        if (done) break
        chunks.push(value)
      }
    } catch { /* closed */ }
    const text = new TextDecoder().decode(concat(chunks))
    for (const line of text.split('\n')) {
      const l = line.trim()
      if (!l) continue
      // Drop anything that arrives after the caller closed this transport
      // (tether#63). ChatPane builds a NEW TetherWT per connect and closes the
      // old one, but a stream already in flight can still resolve afterwards —
      // and since a KindError payload now MUTATES SHARED STATE (store.fatal), a
      // superseded transport's late refusal could otherwise strand a fatal on
      // the healthy connection that replaced it and stop its ladder. Envelopes
      // used to be append-only, which is why this did not matter before.
      if (this.closed) continue
      try {
        const env = JSON.parse(l) as Envelope
        this.opts.onEnvelope?.(env)
      } catch { /* malformed line */ }
    }
  }

  // openBidiStream opens a bidi stream for chat/shell — caller owns read/write.
  async openBidiStream(): Promise<WebTransportBidirectionalStream> {
    if (!this.wt) throw new Error('not connected')
    return this.wt.createBidirectionalStream()
  }

  close(): void {
    this.closed = true
    this.wt?.close({ closeCode: 0, reason: 'client close' })
  }
}

// createWT creates a WebTransport to the given URL with cert pinning.
// For callers that don't need the full TetherWT wrapper (e.g. shell pane).
// Fetches cert hash + WT ticket so the server's ClientIDFromTicket check passes.
export async function createWT(url: string): Promise<WebTransport> {
  const [certHash, ticket] = await Promise.all([fetchCertHash(), fetchWtTicket()])
  const opts: WebTransportOptions = certHash
    ? { serverCertificateHashes: [{ algorithm: 'sha-256', value: hexToBuffer(certHash) }] }
    : {}
  const wt = new WebTransport(appendTicket(url, ticket), opts)
  await wt.ready
  return wt
}

// connectEventsOnly connects to /wt/events?sid=<sid> for read-only fan-out
// attach (multi-tab). clientId is derived server-side from the JWT cookie —
// no need to pass it in the URL. Returns the WebTransport; caller must close it.
export async function connectEventsOnly(
  sid: string,
  onEnvelope: (env: Envelope) => void,
  onClose: () => void,
): Promise<WebTransport> {
  const [certHash, ticket] = await Promise.all([fetchCertHash(), fetchWtTicket()])
  const wtOpts: WebTransportOptions = certHash
    ? { serverCertificateHashes: [{ algorithm: 'sha-256', value: hexToBuffer(certHash) }] }
    : {}
  const url = appendTicket(
    `https://${location.host}/wt/events?sid=${encodeURIComponent(sid)}`,
    ticket,
  )
  const wt = new WebTransport(url, wtOpts)
  await wt.ready

  // Read incoming unidirectional streams — same pattern as TetherWT.readEvents.
  let closeFired = false
  const fireClose = () => {
    if (!closeFired) { closeFired = true; onClose() }
  }
  ;(async () => {
    const reader = wt.incomingUnidirectionalStreams.getReader()
    try {
      while (true) {
        const { value: stream, done } = await reader.read()
        if (done) break
        // Read each stream and parse JSONL envelopes.
        ;(async () => {
          const sr = stream.getReader()
          const chunks: Uint8Array[] = []
          try {
            while (true) {
              const { value, done: sd } = await sr.read()
              if (sd) break
              chunks.push(value)
            }
          } catch { /* closed */ }
          const text = new TextDecoder().decode(concat(chunks))
          for (const line of text.split('\n')) {
            const l = line.trim()
            if (!l) continue
            try { onEnvelope(JSON.parse(l) as Envelope) } catch { /* malformed */ }
          }
        })()
      }
    } catch { /* transport closed */ }
    fireClose()
  })()

  wt.closed.then(fireClose).catch(fireClose)
  return wt
}

// fetchWtTicket fetches a short-lived WT auth ticket from the daemon.
// The browser has the session cookie; the Ticket bridges it to the WT
// CONNECT request which Chrome does not attach cookies to.
// Returns null on network error; redirects to /auth on 401/403.
async function fetchWtTicket(): Promise<string | null> {
  try {
    const resp = await fetch('/api/v1/auth/wt-ticket', { method: 'POST' })
    if (resp.status === 401 || resp.status === 403) {
      // Session cookie missing or expired — send user to the login page.
      window.location.href = '/auth'
      return null
    }
    if (!resp.ok) return null
    const body = await resp.json() as { ticket?: string }
    return typeof body.ticket === 'string' ? body.ticket : null
  } catch {
    return null
  }
}

// appendTicket appends ?ticket=<t> (or &ticket=<t>) to url.
// Returns url unchanged when ticket is null.
function appendTicket(url: string, ticket: string | null): string {
  if (!ticket) return url
  const sep = url.includes('?') ? '&' : '?'
  return `${url}${sep}ticket=${encodeURIComponent(ticket)}`
}

// fetchCertHash fetches /cert-hash from the current origin.
// Returns null if the request fails (e.g. CA-signed cert, no endpoint).
async function fetchCertHash(): Promise<HashHex64 | null> {
  try {
    const resp = await fetch('/cert-hash')
    if (!resp.ok) return null
    const text = (await resp.text()).trim()
    return /^[0-9a-f]{64}$/.test(text) ? text : null
  } catch {
    return null
  }
}

function hexToBuffer(hex: string): ArrayBuffer {
  const bytes = new Uint8Array(32)
  for (let i = 0; i < 32; i++) {
    bytes[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16)
  }
  return bytes.buffer
}

function concat(chunks: Uint8Array[]): Uint8Array {
  const total = chunks.reduce((n, c) => n + c.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const c of chunks) { out.set(c, offset); offset += c.length }
  return out
}
