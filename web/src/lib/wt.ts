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
    const certHash = await fetchCertHash()
    const wtOpts: WebTransportOptions = certHash
      ? { serverCertificateHashes: [{ algorithm: 'sha-256', value: hexToBuffer(certHash) }] }
      : {}
    this.wt = new WebTransport(this.opts.url, wtOpts)
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
