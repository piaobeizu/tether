import { describe, expect, it } from 'vitest'
import { safeRedirectTarget } from './AuthPage'

const ORIGIN = 'https://tether.example'

describe('safeRedirectTarget (tether#117 A3)', () => {
  it('keeps a same-origin path, with its query and hash', () => {
    expect(safeRedirectTarget('/work', ORIGIN)).toBe('/work')
    expect(safeRedirectTarget('/work?id=7', ORIGIN)).toBe('/work?id=7')
    expect(safeRedirectTarget('/work?id=7#tab', ORIGIN)).toBe('/work?id=7#tab')
    expect(safeRedirectTarget('/', ORIGIN)).toBe('/')
  })

  it('falls back to / when there is no redirect at all', () => {
    expect(safeRedirectTarget(null, ORIGIN)).toBe('/')
    expect(safeRedirectTarget('', ORIGIN)).toBe('/')
  })

  // The three vectors from the wi. The first was already caught by the old
  // guard; the other two are the ones that were not, and `/\` was reproduced
  // navigating off-site in real headless Chrome against the old code.
  it.each([
    ['protocol-relative', '//evil.example'],
    ['backslash after the slash', '/\\evil.example'],
    ['backslash pair', '/\\/evil.example'],
    ['tab between the slashes', '/\t/evil.example'],
    ['newline between the slashes', '/\n/evil.example'],
    ['carriage return between the slashes', '/\r/evil.example'],
  ])('rejects %s', (_label, raw) => {
    const got = safeRedirectTarget(raw, ORIGIN)
    expect(got).toBe('/')
    // Belt and braces: whatever comes back must not resolve off-origin either.
    expect(new URL(got, ORIGIN).origin).toBe(ORIGIN)
  })

  it('rejects an absolute URL to another origin', () => {
    expect(safeRedirectTarget('https://evil.example/x', ORIGIN)).toBe('/')
    expect(safeRedirectTarget('http://tether.example/x', ORIGIN)).toBe('/') // scheme downgrade
    expect(safeRedirectTarget('https://tether.example.evil/x', ORIGIN)).toBe('/')
    expect(safeRedirectTarget('https://tether.example:8443/x', ORIGIN)).toBe('/') // port is part of the origin
  })

  it('rejects opaque schemes, whose origin is not an origin', () => {
    expect(safeRedirectTarget('javascript:alert(1)', ORIGIN)).toBe('/')
    expect(safeRedirectTarget('data:text/html,<script>alert(1)</script>', ORIGIN)).toBe('/')
    expect(safeRedirectTarget('blob:https://evil.example/x', ORIGIN)).toBe('/')
  })

  it('never returns a value carrying an origin, whatever it is handed', () => {
    const inputs = [
      '/ok',
      '//evil.example',
      '/\\evil.example',
      'https://evil.example',
      'javascript:alert(1)',
      '/\t/evil.example',
      '////evil.example',
      '/%2F%2Fevil.example',
      '\\\\evil.example',
    ]
    for (const raw of inputs) {
      const got = safeRedirectTarget(raw, ORIGIN)
      expect(got.startsWith('/')).toBe(true)
      expect(got.startsWith('//')).toBe(false)
      expect(new URL(got, ORIGIN).origin).toBe(ORIGIN)
    }
  })

  // The round trip the feature exists for: redirectToAuth() in lib/auth.ts
  // encodeURIComponent's the current path+search, and URLSearchParams decodes it
  // on the way back in. An encoded path must survive that unchanged.
  it('round-trips what redirectToAuth produces', () => {
    const target = '/work?wi=tether%23117&q=a b'
    const search = `?redirect=${encodeURIComponent(target)}`
    const raw = new URLSearchParams(search).get('redirect')
    expect(safeRedirectTarget(raw, ORIGIN)).toBe('/work?wi=tether%23117&q=a%20b')
  })
})
