/**
 * Resolve the post-login `?redirect=` target to a same-origin path, or `/`.
 *
 * The producers are real and must keep working. `signInURL()` in
 * internal/auth/oauth/handlers.go puts a whole authorization request here so a
 * signed-out browser comes back to it (tether#153), and the SPA's
 * `redirectToAuth()` put the current path + query here on a 401 so the owner
 * landed back where they were — that one lived in `lib/auth.ts` before the
 * phase-1 rewrite (tether#173) and has not been ported back yet, so this guard
 * has no in-tree JS caller today. (A review pass called this parameter unused —
 * it had grepped only the Go side.)
 *
 * Which is why this lives in lib/ and not inside a login page: the guard is
 * decided and its two failure shapes are paid for, while the page that will call
 * it is not designed yet. tether#185 moved it out of `AuthPage.tsx` for that
 * reason, and because the old filename made a reader assume this was a component
 * test.
 *
 * The previous guard was `raw.startsWith('/') && !raw.startsWith('//')`, and it
 * did not hold: WHATWG URL parsing treats `\` as `/` for special schemes, so
 * `/\evil.example` resolves to `https://evil.example/`. Verified in headless
 * Chrome against that exact code — `?redirect=//host` was contained, and
 * `?redirect=/\/host` navigated off-site. (That is tether#117 A3's own
 * measurement, carried over verbatim and NOT re-verified by tether#185: the box
 * this was moved on has no display server, and the suite next door runs under
 * jsdom, i.e. Node's WHATWG URL implementation — the same spec, a different
 * implementation.) `/<TAB>/evil.example` slips through the same way, because
 * tabs are stripped before parsing.
 *
 * So the check is not "does this string look relative" but "where does this
 * actually resolve to", answered by the same parser the browser will use for the
 * navigation.
 *
 * `url.origin === origin` is necessary but NOT sufficient, which is the trap the
 * first attempt at this fix fell into: `url.pathname` is not guaranteed to be a
 * same-origin path. `..` segments collapse during parsing, so
 * `/..//evil.example` yields origin=<ours> with pathname=`//evil.example` — and
 * THAT string, assigned to location.href, is protocol-relative and leaves the
 * site. `/./\evil.example` and `https://<our-host>//evil.example` do the same.
 *
 * So this validates and otherwise bails, rather than trying to repair the input.
 * The guard has been wrong twice by being clever about transforming a string;
 * every branch below is a refusal, and `/` is always a safe answer. The
 * property — that no input can produce an off-origin return value — is asserted
 * over a generated corpus in safeRedirectTarget.test.ts rather than argued here.
 *
 * That file is only half the gate, and the halves are not interchangeable. It
 * generates adversarial input and asserts nothing escapes, so it answers "does
 * this refuse correctly" — and a degenerate implementation returning `/` for
 * every input satisfies every one of its assertions. oauthSignInRedirect.test.ts
 * next to it answers the other direction, "does this ACCEPT what the real
 * producer emits", by running the Go-side corpus through this function and
 * asserting the result is not `/`. Do not merge them; each is blind to what the
 * other catches.
 */
export function safeRedirectTarget(raw: string | null, origin: string): string {
  if (!raw) return '/'
  try {
    const url = new URL(raw, origin)
    // Covers off-origin hosts, scheme changes, and opaque schemes such as
    // javascript: and data:, whose origin is the string "null".
    if (url.origin !== origin) return '/'
    const target = url.pathname + url.search + url.hash
    // A path that does not start with a slash is not a path at all (a same-origin
    // blob: URL puts its whole inner URL in pathname); one that starts with two
    // is an authority. Anything else cannot be re-read as an origin, and neither
    // a query nor a fragment can introduce one after a path has begun.
    if (!target.startsWith('/') || target.startsWith('//')) return '/'
    return target
  } catch {
    return '/'
  }
}
