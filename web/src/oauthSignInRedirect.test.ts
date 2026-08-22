import { describe, expect, it } from 'vitest'
import { safeRedirectTarget } from './AuthPage'
import corpus from '../../internal/auth/oauth/testdata/signin_redirect_corpus.json'

// tether#153 — the consumer half of the sign-in redirect contract.
//
// The producer is signInURL() in internal/auth/oauth/handlers.go: when a
// signed-out browser opens GET /oauth/authorize it now gets a sign-in prompt
// whose link is /auth?redirect=<the whole authorization request>. This file is
// the other end of that link — the strings the Go handler emits, run through the
// real safeRedirectTarget, which is what AuthPage actually navigates with.
//
// The corpus is a file both sides READ. tether#117 A3 is the reason: `url.origin
// === origin` is necessary but not sufficient, and the two shapes that proved it
// — `..` segments and same-origin absolute URLs — are only interesting if the
// same bytes reach both languages. Two hand-copied lists would each hold a
// different subset while both looked complete. internal/auth/oauth/
// signin_prompt_test.go asserts the handler still produces these exact strings,
// so the file cannot go stale without a red test on the Go side.

const ORIGIN = 'https://tether.example'

// A redirect the guard refuses comes back as '/', which is safe but useless: the
// consent page would be lost and the user would land on the home screen, which
// is the bug tether#153 exists to fix. So "not '/'" is a real assertion here,
// not a tautology.
const REJECTED = '/'

describe('the tether#153 sign-in redirect survives safeRedirectTarget', () => {
  it('has a populated corpus covering both tether#117 A3 shapes', () => {
    // Guard the guard: an empty corpus turns every it.each below into zero
    // tests, and a green run of nothing looks exactly like a green run.
    expect(corpus.entries.length).toBeGreaterThanOrEqual(8)
    const labels = corpus.entries.map(e => e.label)
    expect(labels).toContain('dot-dot-segments-raw-in-a-value')
    expect(labels).toContain('dot-dot-segments-percent-encoded-in-redirect-uri')
    expect(labels).toContain('same-origin-absolute-url-raw')
    expect(labels).toContain('same-origin-absolute-url-percent-encoded')
  })

  it.each(corpus.entries.map(e => [e.label, e] as const))('%s', (_label, entry) => {
    // The browser's own decode path, not a hand-rolled one: AuthPage reads
    // window.location.search through URLSearchParams, so the value handed to
    // safeRedirectTarget is whatever that produces from the Go-built href.
    const authURL = new URL(entry.wantAuthURL, ORIGIN)
    expect(authURL.pathname).toBe('/auth')
    const redirect = authURL.searchParams.get('redirect')
    expect(redirect).not.toBeNull()

    const got = safeRedirectTarget(redirect, ORIGIN)

    // 1. It is accepted. A rejected target means the user signs in and lands on
    //    the home screen with the authorization request gone — the original bug,
    //    reintroduced one layer down.
    expect(got).not.toBe(REJECTED)
    expect(got.startsWith('/oauth/authorize')).toBe(true)

    // 2. It is safe. The same property AuthPage.test.tsx asserts over its own
    //    generated corpus, restated over the strings this producer emits: no
    //    input may yield something that leaves the origin.
    expect(got.startsWith('//')).toBe(false)
    expect(new URL(got, ORIGIN).origin).toBe(ORIGIN)

    // 3. The query survives. This is what the redirect is FOR: client_id,
    //    code_challenge, state and redirect_uri have to come back or the consent
    //    page renders against a request that no longer says anything.
    const back = new URL(got, ORIGIN)
    for (const [key, want] of Object.entries(entry.expectParams ?? {})) {
      expect(back.searchParams.get(key)).toBe(want)
    }
  })

  // Named separately from the loop because it is the assertion the loop's
  // per-entry checks are easy to satisfy vacuously: if the producer ever dropped
  // the query and emitted a bare '/oauth/authorize', every safety check above
  // would still pass and only this would notice.
  it('carries a query on every entry, not a bare path', () => {
    for (const entry of corpus.entries) {
      const redirect = new URL(entry.wantAuthURL, ORIGIN).searchParams.get('redirect')
      const got = safeRedirectTarget(redirect, ORIGIN)
      expect(got, `${entry.label}: ${got}`).toContain('?')
      expect(new URL(got, ORIGIN).searchParams.get('response_type')).toBe('code')
    }
  })
})
