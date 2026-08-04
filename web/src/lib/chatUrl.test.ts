import { describe, expect, it } from 'vitest'
import { chatURL } from './chatUrl'

// tether#52 — chatURL is the pure seam for the /wt/chat connect URL. The rule
// under test in most of these — `ws` travels ONLY when `sid` is absent — is
// the load-bearing one: see chatUrl.ts's doc comment for why getting this
// backwards can silently abandon a live session mid-turn.

describe('chatURL (tether#52)', () => {
  it('always includes provider, with no sid/ws when neither is given', () => {
    expect(chatURL({ host: 'h.example', provider: 'claude-code' }))
      .toBe('https://h.example/wt/chat?provider=claude-code')
  })

  it('includes sid when non-empty and no workspace was given', () => {
    expect(chatURL({ host: 'h.example', provider: 'claude-code', sid: 'sid-1' }))
      .toBe('https://h.example/wt/chat?provider=claude-code&sid=sid-1')
  })

  it('includes ws when sid is empty/absent and wsID is non-empty (new-session path)', () => {
    expect(chatURL({ host: 'h.example', provider: 'claude-code', wsID: 'ws-1' }))
      .toBe('https://h.example/wt/chat?provider=claude-code&ws=ws-1')
    expect(chatURL({ host: 'h.example', provider: 'claude-code', sid: '', wsID: 'ws-1' }))
      .toBe('https://h.example/wt/chat?provider=claude-code&ws=ws-1')
  })

  // THE load-bearing negative: a sid present means the daemon already knows
  // that session's workspace, so ws must never ride along — see chatUrl.ts.
  it('omits ws when sid is present, even if wsID is also given', () => {
    const url = chatURL({ host: 'h.example', provider: 'claude-code', sid: 'sid-1', wsID: 'ws-1' })
    expect(url).toBe('https://h.example/wt/chat?provider=claude-code&sid=sid-1')
    expect(url).not.toContain('ws=')
  })

  it('never emits an empty-valued sid or ws param', () => {
    const url = chatURL({ host: 'h.example', provider: 'claude-code', sid: '', wsID: '' })
    expect(url).toBe('https://h.example/wt/chat?provider=claude-code')
    expect(url).not.toContain('sid=')
    expect(url).not.toContain('ws=')
  })

  it('percent-encodes provider, sid, and ws values', () => {
    const url = chatURL({ host: 'h.example', provider: 'a/b c', sid: 'x?y=z' })
    expect(url).toBe(`https://h.example/wt/chat?provider=${encodeURIComponent('a/b c')}&sid=${encodeURIComponent('x?y=z')}`)
    expect(url).toContain('a%2Fb%20c')
    expect(url).toContain('x%3Fy%3Dz')

    const wsUrl = chatURL({ host: 'h.example', provider: 'claude-code', wsID: '工作区/1' })
    expect(wsUrl).toContain(`ws=${encodeURIComponent('工作区/1')}`)
  })

  it('builds the https URL from host verbatim', () => {
    expect(chatURL({ host: 'localhost:5173', provider: 'claude-code' }))
      .toBe('https://localhost:5173/wt/chat?provider=claude-code')
  })
})
