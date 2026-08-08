import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import ChatPane from './index'
import { useStore } from '../../lib/store'

// tether#80 — the LAST hop, which nothing pinned until now.
//
// Everything else about a notice is asserted in store.test.ts: it is created,
// it is bounded, it is ordered, and mergeTranscript projects it into a
// `role: 'system'` Message. None of that says the pane still CALLS
// mergeTranscript. ChatPane.test.tsx is 560 lines that render sub-components
// directly and never mount ChatPane at all, so replacing
// `mergeTranscript(messages, notices)` with `messages` at index.tsx's transcript
// memo left the entire frontend suite green — every notice in the app silently
// stops being rendered and no test notices (found by tether#77's review, filed
// as this wi's N5; the general form is the team's
// "test the WIRING hop, not just the unit").
//
// That gap is worth more than usual here, because "the frame becomes a line a
// human can read" is the whole point of this wi. A fix that produced a perfect
// notice the pane never rendered would pass every other test in the repo.
//
// Mounting ChatPane is possible without a WebTransport stack for one specific
// reason: with no remembered sid and `workspacesLoaded` false, the first connect
// is DEFERRED (shouldDeferFirstConnect) behind a store subscription and a 2s
// fallback timer, neither of which fires here. That is the harness's own
// precondition, so it is asserted below rather than assumed — if a future change
// makes the pane connect on mount, these tests must fail loudly instead of
// quietly exercising a different path.

const originalFetch = globalThis.fetch

function stubFetch() {
  // Both mount fetches: GET /api/v1/providers, and GET /messages (never reached
  // here — sessionId stays null — but stubbed so a regression that DOES reach it
  // fails on an assertion rather than an unhandled rejection).
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
    const body = url.includes('/providers') ? { providers: ['claude-code'] } : []
    return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
  })
}

describe('ChatPane renders notices (tether#80, wi N5)', () => {
  beforeEach(() => {
    localStorage.clear()
    globalThis.fetch = stubFetch() as unknown as typeof fetch
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: null, workspacesLoaded: false, activeWorkspace: null,
    })
    // The harness's precondition, asserted: no remembered sid + workspaces not
    // settled is exactly the state that defers the first connect.
    expect(localStorage.getItem('tether_last_sid')).toBeNull()
    expect(useStore.getState().workspacesLoaded).toBe(false)
    // And nothing in this file may reach a WebTransport constructor.
    expect((globalThis as { WebTransport?: unknown }).WebTransport).toBeUndefined()
  })

  afterEach(() => {
    cleanup()
    globalThis.fetch = originalFetch
    useStore.setState({ messages: [], notices: [], sessionId: null })
    localStorage.clear()
  })

  it('renders an agent-error notice as a system line in the transcript', () => {
    useStore.setState({
      notices: [{
        id: 'n1',
        text: 'The agent reported an error — busy: another prompt is running',
        ts: 1000,
        kind: 'agent_error',
        repeats: 1,
      }],
    })
    render(<ChatPane />)
    const line = screen.getByText('The agent reported an error — busy: another prompt is running')
    expect(line).toBeTruthy()
    expect(line.className).toBe('msg-system-text')
  })

  it('renders the collapsed repeat count the store folded arrivals into', () => {
    useStore.setState({
      notices: [{ id: 'n1', text: 'The agent reported an error — busy', ts: 1000, kind: 'agent_error', repeats: 12 }],
    })
    render(<ChatPane />)
    // The count lives in the mergeTranscript projection, so this also pins that
    // the pane renders the PROJECTION and not Notice.text directly.
    expect(screen.getByText('The agent reported an error — busy (×12)')).toBeTruthy()
  })

  it('interleaves the notice with the conversation instead of dropping either', () => {
    useStore.setState({
      messages: [
        { id: 'u1', role: 'user', text: 'first prompt', ts: 1000 },
        { id: 'u2', role: 'user', text: 'second prompt', ts: 3000 },
      ],
      notices: [{ id: 'n1', text: 'The agent reported an error — boom', ts: 2000, kind: 'agent_error' }],
    })
    const { container } = render(<ChatPane />)
    const texts = [...container.querySelectorAll('.msg-user-bubble, .msg-system-text')].map((e) => e.textContent)
    expect(texts).toEqual(['first prompt', 'The agent reported an error — boom', 'second prompt'])
  })

  // The other direction: an empty notice list must not invent a system line.
  //
  // NEGATIVE CONTROL — this one passes with or without the wiring hop, by
  // construction (`messages` and `mergeTranscript(messages, [])` are the same
  // array). It is here to say that the three above discriminate and this does
  // not, so nobody later reads "4 passed" as four independent guards.
  it('renders no system line when there are no notices', () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }], notices: [] })
    const { container } = render(<ChatPane />)
    expect(container.querySelectorAll('.msg-system')).toHaveLength(0)
    expect(container.querySelectorAll('.msg-user-bubble')).toHaveLength(1)
  })
})
