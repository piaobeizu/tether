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

  // `url.origin === location.origin` is necessary but not sufficient, and these
  // are the inputs that prove it. Every one parses to OUR origin while leaving
  // `url.pathname` protocol-relative, because `..` segments collapse — so a
  // guard that returned `url.pathname` unchanged handed back `//evil.example`
  // and the navigation left the site. Found by review, not by the corpus above,
  // which is the point: the suite's strongest assertion
  // (`got.startsWith('//') === false`) was never handed an input that could
  // violate it.
  it.each([
    ['dot-dot collapsing into a double slash', '/..//evil.example'],
    ['dot-dot with a path and query', '/..//evil.example/steal?t=1'],
    ['single-dot then backslash', '/./\\evil.example'],
    ['deeper dot-dot climb', '/a/../..//evil.example'],
    ['absolute same-origin URL with a double-slash path', 'https://tether.example//evil.example'],
    ['protocol-relative to our own host, then off-host', '//tether.example//evil.example'],
    ['absolute same-origin URL with double backslash', 'https://tether.example\\\\evil.example'],
    ['many leading slashes after a climb', '/..////evil.example'],
  ])('rejects %s (same origin, off-origin pathname)', (_label, raw) => {
    const got = safeRedirectTarget(raw, ORIGIN)
    expect(new URL(got, ORIGIN).origin).toBe(ORIGIN)
    expect(got.startsWith('//')).toBe(false)
  })

  // A generated corpus, so the trigger set is not a hand-picked list that
  // happens to miss the next parser quirk. The assertion is the only thing that
  // actually matters: whatever is returned, resolved by the same parser the
  // browser uses, must stay on this origin.
  it('never escapes the origin, over a generated cross-product', () => {
    const prefixes = ['', '/', '//', '/..', '/../', '/..//', '/.', '/./', '/a/../..', '\\', '/\\', '/\t', '/\n', '////']
    const middles = ['', '/', 'evil.example', '/evil.example', '//evil.example', 'tether.example//evil.example']
    const suffixes = ['', '/steal', '?t=1', '#f', '/steal?t=1#f']
    const schemes = ['', 'https://tether.example', 'https://evil.example', 'javascript:', 'data:text/html,']
    let checked = 0
    for (const s of schemes) {
      for (const p of prefixes) {
        for (const m of middles) {
          for (const suf of suffixes) {
            const raw = s + p + m + suf
            const got = safeRedirectTarget(raw, ORIGIN)
            checked++
            // One assertion, stated as the property rather than as an expected
            // value: it holds for every input, so a new quirk cannot slip past
            // by not being in a list.
            if (new URL(got, ORIGIN).origin !== ORIGIN || got.startsWith('//')) {
              throw new Error(
                `safeRedirectTarget(${JSON.stringify(raw)}) = ${JSON.stringify(got)} escapes ${ORIGIN}`,
              )
            }
          }
        }
      }
    }
    // Guard the guard: if the loops ever stop generating, the test must not
    // quietly become a no-op that passes.
    expect(checked).toBe(schemes.length * prefixes.length * middles.length * suffixes.length)
    expect(checked).toBeGreaterThan(1000)
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
      '/..//evil.example',
      // A blob URL of OUR origin: url.origin matches and url.pathname is the
      // whole inner URL, "https://tether.example/x". Navigating there would not
      // leave the origin, so it is not an escape — but it is not a path either,
      // and "the return value is always a same-origin path" is the invariant
      // this function documents. The post-condition is what makes that true
      // rather than nearly true.
      'blob:https://tether.example/x',
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
