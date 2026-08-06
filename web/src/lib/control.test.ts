import { describe, it, expect, beforeEach } from 'vitest'
import { ControlClient, activeControlClient } from './control'
import { ClientFrameResize } from './wire.gen'
import type { ClientFrame } from './wire.gen'

/**
 * These cover the send side of the terminal-resize lane (tether#68): that a
 * resize actually leaves as a well-formed frame carrying the numbers it was
 * given. The browser half (ResizeObserver → xterm onResize → here) needs a
 * real layout engine and is covered by live-verify instead.
 */

/** Captures everything written to the control stream, decoded to frames. */
function captureWriter(client: ControlClient): ClientFrame[] {
  const sent: ClientFrame[] = []
  const writer = {
    write(bytes: Uint8Array) {
      for (const line of new TextDecoder().decode(bytes).split('\n')) {
        if (line.trim()) sent.push(JSON.parse(line) as ClientFrame)
      }
      return Promise.resolve()
    },
  }
  // The writer is private and only ever set by connect(), which needs a live
  // WebTransport; inject directly so the frame shape can be asserted without one.
  ;(client as unknown as { writer: unknown }).writer = writer
  return sent
}

describe('ControlClient.sendResize', () => {
  let client: ControlClient
  let sent: ClientFrame[]

  beforeEach(() => {
    client = new ControlClient()
    sent = captureWriter(client)
  })

  it('sends a resize frame carrying the session id and the given dimensions', async () => {
    await client.sendResize('sid-42', 143, 41)

    expect(sent).toHaveLength(1)
    expect(sent[0]).toEqual({
      kind: ClientFrameResize,
      sessionId: 'sid-42',
      cols: 143,
      rows: 41,
    })
  })

  // 143 !== 41 above, so a transposed cols/rows would fail that assertion; this
  // pins the orientation explicitly so the intent survives a future refactor.
  it('does not transpose cols and rows', async () => {
    await client.sendResize('s', 200, 50)
    expect(sent[0].cols).toBe(200)
    expect(sent[0].rows).toBe(50)
  })

  it('keeps the empty session id — a shell can exist before any chat session', async () => {
    await client.sendResize('', 80, 24)
    expect(sent).toHaveLength(1)
    expect(sent[0].sessionId).toBe('')
  })

  it.each([
    ['zero cols', 0, 24],
    ['zero rows', 80, 0],
    ['both zero', 0, 0],
    ['negative', -1, 24],
  ])('drops %s instead of blanking the remote TUI', async (_name, cols, rows) => {
    await client.sendResize('sid', cols, rows)
    expect(sent).toHaveLength(0)
  })

  it('is a no-op when the control lane is not connected', async () => {
    const disconnected = new ControlClient()
    await expect(disconnected.sendResize('sid', 80, 24)).resolves.toBeUndefined()
  })
})

describe('activeControlClient', () => {
  it('is null until a client starts', () => {
    expect(activeControlClient()).toBeNull()
  })

  it('publishes the started client and clears it on stop', async () => {
    const client = new ControlClient()
    // start() attempts a real WT connect, which fails under vitest and is
    // swallowed into a reconnect timer — publication happens before that, which
    // is the behaviour ShellPane depends on.
    await client.start()
    expect(activeControlClient()).toBe(client)

    client.stop()
    expect(activeControlClient()).toBeNull()
  })

  it('stop() on a superseded client does not clear the live one', async () => {
    const first = new ControlClient()
    await first.start()
    const second = new ControlClient()
    await second.start()

    first.stop()

    expect(activeControlClient()).toBe(second)
    second.stop()
  })
})
