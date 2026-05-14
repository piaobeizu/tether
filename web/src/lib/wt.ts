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

export class TetherWT {
  private wt: WebTransport | null = null
  private opts: WTOptions
  private closed = false

  constructor(opts: WTOptions) {
    this.opts = opts
  }

  async connect(): Promise<void> {
    const [certHash, ticket] = await Promise.all([fetchCertHash(), fetchWtTicket()])
    const wtOpts: WebTransportOptions = certHash
      ? { serverCertificateHashes: [{ algorithm: 'sha-256', value: hexToBuffer(certHash) }] }
      : {}
    this.wt = new WebTransport(appendTicket(this.opts.url, ticket), wtOpts)
    await this.wt.ready
    this.readEvents()
    this.wt.closed.then((info) => {
      if (!this.closed) this.opts.onClose?.(info.closeCode ?? 0, info.reason ?? '')
    }).catch(() => {})
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
