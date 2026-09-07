// The one breakpoint the shell has (tether#195, owner ruling ②).
//
// At `lg` and above the shell is three columns; below it, one. There is exactly
// one size breakpoint on purpose — the old SPA also had exactly one
// (`@media (max-width: 768px)` in `web/src/index.css` **on `main`**, where that
// file still exists; its four other `@media` blocks were all
// `prefers-reduced-motion`), and a second one is a design decision, not a detail.
//
// The branch qualifier is not pedantry: that path does not resolve on THIS branch,
// because tether#174 deleted it, and a path cited without saying which branch it
// was read on is wrong the moment the two branches differ.
//
// 🔴 `LG_MIN_WIDTH` is a COPY of a value the vendored Tailwind config owns, and
// it is a copy for a reason that does not extend to letting it drift.
//
//   Why a copy: resolving the Tailwind config at runtime would pull
//   `tailwindcss/resolveConfig`, the vendored config and its `createRequire`
//   shim into the browser bundle, to learn one integer that is fixed at build
//   time.
//   Why it cannot drift: breakpoint.test.ts pins it in TWO steps whose
//   conjunction is the guarantee. There is no `resolveConfig` call anywhere —
//   this description used to say the test "resolves the config tether actually
//   builds with … the config is the authority", and it does neither, which sent
//   a reader hunting for a call that is not there.
//     · step 1 (`equals Tailwind's lg`) compares this constant against
//       `tailwindcss/defaultTheme`'s `screens.lg`, so a Tailwind upgrade that
//       moved `lg` reddens instead of leaving the shell branching at a width its
//       own stylesheet no longer uses.
//     · step 2 (`is not overridden by either Tailwind config`) reads both configs
//       as `?raw` TEXT — the vendored one and web/src/ui/tailwind.config.mjs, the
//       file postcss actually loads — and asserts neither declares
//       `theme.screens`. That is what makes step 1's default the EFFECTIVE value
//       rather than merely a default.
//   So the authority is `defaultTheme`, conditional on the configs staying quiet
//   about `screens`. If either ever declares `theme.screens`, step 2 fails and
//   this constant has to be derived from the resolved config instead.
//   Check: `cd web && pnpm vitest run src/shell/breakpoint.test.ts`
//
// Stated in px rather than as a Tailwind class because the shell branches on it
// in JavaScript — which panes are MOUNTED differs between the two forms
// (LAY-14), and `display: none` is not that.
export const LG_MIN_WIDTH = 1024

/** The media query the shell listens on. */
export const LG_MEDIA_QUERY = `(min-width: ${LG_MIN_WIDTH}px)`

/**
 * A subscription to "is the viewport at least `lg`".
 *
 * Split out of the React hook so the semantics can be tested without a renderer,
 * and so the hook has nothing in it but `useSyncExternalStore` wiring.
 *
 * A browser with no `matchMedia` (a very old one, or a non-DOM host) reports
 * `false` — the narrow layout. That is the safe direction and it is not a guess
 * dressed as knowledge: the narrow form renders every pane the wide form does,
 * one at a time, so a wrong answer here costs layout, not access. The reverse
 * default would put a three-column shell on a phone.
 */
export interface WideSubscription {
  isWide(): boolean
  subscribe(onChange: () => void): () => void
}

export function createWideSubscription(win: Window | undefined = globalThis.window): WideSubscription {
  const mql = win && typeof win.matchMedia === 'function' ? win.matchMedia(LG_MEDIA_QUERY) : null

  return {
    isWide: () => mql?.matches ?? false,
    subscribe(onChange) {
      if (!mql) return () => {}
      // `addEventListener` on a MediaQueryList is the current API; `addListener`
      // is the deprecated one some older WebKit builds still only have. Both are
      // handled because falling back to "never updates" would mean a rotation
      // from portrait to landscape leaves the shell in the wrong form until the
      // next reload, which is exactly the mobile case this wi is for.
      if (typeof mql.addEventListener === 'function') {
        mql.addEventListener('change', onChange)
        return () => mql.removeEventListener('change', onChange)
      }
      if (typeof mql.addListener === 'function') {
        mql.addListener(onChange)
        return () => mql.removeListener(onChange)
      }
      return () => {}
    },
  }
}
