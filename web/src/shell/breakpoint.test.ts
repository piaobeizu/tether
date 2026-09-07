/// <reference types="vite/client" />
// The shell's breakpoint is duplicated in three places that cannot import each
// other — a TypeScript constant, a CSS media query, and Tailwind's `lg` — so this
// file is what stops them drifting. Each assertion compares the copy against the
// authority, never two copies against each other.

import { describe, expect, it, vi } from 'vitest'
import defaultTheme from 'tailwindcss/defaultTheme'
import { createWideSubscription, LG_MEDIA_QUERY, LG_MIN_WIDTH } from './breakpoint'
import { stripCssComments } from './cssText'

// `?raw` through vite's glob, so the assertions read the files that ship rather
// than a fixture. A file added to either directory later is picked up without
// being added to a list.
//
// 🔴 `../ui/*.css` is in the set, and it was NOT at first. Scoped to `./*.css`
// alone this glob resolved to exactly `['./shell.css']`, so web/src/ui/index.css —
// the wrap layer, which tether#195 modifies — sat OUTSIDE the gate: appending
// `@media (min-width: 900px){…}` to it left this file green (measured, before the
// glob was widened). The gate has to cover every stylesheet tether writes, not
// just the one the rule was first written for.
//
// The vendored sheet is deliberately NOT in the set. It is upstream's, may not be
// edited, and it does carry width media queries of its own — including a
// `max-width: 768px` block whose rules for `*` / `button` / `[role="button"]` / `a`
// DO reach the shell's controls. Pulling it in would make this gate assert
// something about upstream that tether cannot act on. The claim is scoped to
// tether's own stylesheets and shell.css's header states it in that scoped form.
const sources = import.meta.glob(['./*.css', '../ui/*.css'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

/**
 * The same stylesheets with comments removed, which is what every rule below
 * about the CONTENT of a stylesheet has to read.
 *
 * 🔴 Not an optimisation. shell.css's header quotes the exact pattern this file
 * bans — it has to, because the header is where the "no width media query" rule
 * and the `grep` that verifies its scope are written down — and reading raw text
 * made every one of those sentences a false positive. Two of these rules failed
 * on prose the moment that header was corrected. The stripper is shared with
 * mobileFirst.test.ts (./cssText.ts) and guarded by that file's
 * "removes comments and nothing else" case, because a stripper that ate a
 * declaration would turn all of these green at once.
 */
const declarations = (): Record<string, string> =>
  Object.fromEntries(Object.entries(sources).map(([path, css]) => [path, stripCssComments(css)]))

const tailwindConfigs = import.meta.glob(['../ui/tailwind.config.mjs', '../vendor/**/tailwind.config.js'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

describe('breakpoint', () => {
  // Tailwind is the authority for what `lg` means, because the shell's markup and
  // the vendored primitives use its scale. Asserting the shell's constant against
  // `defaultTheme` rather than against the literal 1024 means a Tailwind upgrade
  // that moved `lg` would redden here instead of leaving the shell branching at a
  // width its own stylesheet no longer uses.
  it("equals Tailwind's lg", () => {
    const screens = (defaultTheme as { screens: Record<string, string> }).screens
    expect(screens.lg).toBe(`${LG_MIN_WIDTH}px`)
  })

  // The step above is only sound while nothing overrides `theme.screens`. Neither
  // config may: the vendored one is upstream's and unmodifiable, and the wrap
  // changes exactly one key (`content`). If either grows a `theme.screens`, this
  // fails and the constant has to be derived from the resolved config instead.
  it('is not overridden by either Tailwind config', () => {
    expect(Object.keys(tailwindConfigs).length).toBeGreaterThan(0)
    for (const [path, text] of Object.entries(tailwindConfigs)) {
      // `container.screens` is a different key and upstream does set it; the one
      // that would move the breakpoint is `theme.screens`.
      const withoutContainer = text.replace(/container:\s*\{[\s\S]*?\n {4}\},/g, '')
      expect(withoutContainer, `${path} declares theme.screens`).not.toMatch(
        /^\s{2,6}screens:\s*\{/m,
      )
    }
  })

  // 🔴 The breakpoint exists ONCE, in TypeScript. A width media query in a
  // stylesheet tether writes would be a second copy of it, free to disagree — the
  // layout switching at one width while LAY-14's mounting switched at another,
  // since the two forms are different trees and `display` cannot express a mount.
  // So the assertion is not "the query matches the constant", it is "there is no
  // query".
  //
  // Reading the shipped stylesheets rather than a fixture is what makes this hold
  // for a file added later: the globs are resolved against the real directories.
  it("is not duplicated into any stylesheet tether writes — none has a width media query", () => {
    // Both halves of the glob have to have resolved. One emptiness guard over the
    // union would be satisfied by shell.css alone, which is exactly the state
    // this case was in before `../ui/*.css` was added: green, and blind to the
    // file the PR was editing.
    const paths = Object.keys(sources)
    expect(paths.filter(p => p.startsWith('./')), 'the shell glob resolved to nothing').not.toEqual(
      [],
    )
    expect(paths.filter(p => p.includes('/ui/')), 'the wrap-layer glob resolved to nothing').not.toEqual(
      [],
    )
    for (const [path, raw] of Object.entries(sources)) {
      // The emptiness check is on the RAW text: the stub state vite.config.ts's
      // `test.css` guards against yields '', and a stripped '' is also '', so
      // checking after the strip could not tell "nothing was read" from
      // "everything was a comment".
      expect(raw.length, `${path} read back empty — the raw import is stubbed`).toBeGreaterThan(0)
      const css = declarations()[path]!
      const widthQueries = [...css.matchAll(/@media[^{]*\((?:min|max)-width:[^)]*\)/g)].map(
        m => m[0],
      )
      expect(widthQueries, `${path} declares a width breakpoint`).toEqual([])
    }
  })

  // The switch the stylesheet does use has to be the one the shell publishes, or
  // the wide rules are dead code. Read off the declarations, not the prose: the
  // header discusses the selector at length, so a raw `toContain` would pass on a
  // file whose wide block had been deleted.
  it('drives the wide layout from the attribute the shell publishes', () => {
    expect(declarations()['./shell.css']!).toContain("[data-wide='true']")
  })

  // The narrow layout is what a browser applies before any JavaScript runs, so
  // "mobile-first" is a property of where the rules sit rather than a claim.
  it("keeps the wide rules behind the attribute, so the default cascade is the narrow form", () => {
    // 🔴 Declarations, not raw text. This case slices the file at the FIRST
    // occurrence of the selector, and the header names the selector while
    // explaining which element carries the attribute — so on raw text the slice
    // point landed inside a comment and the case failed on a correct file. Worse
    // than failing: had the header happened to mention it after the rules, the
    // slice would have covered the whole file and the case would have passed
    // vacuously.
    const css = declarations()['./shell.css']!
    const wideBlockStart = css.indexOf("[data-wide='true']")
    expect(wideBlockStart).toBeGreaterThan(-1)
    // `.sh-root`'s sizing is unconditional; the activity bar and the tab strip are
    // hidden by default and only revealed inside the wide block.
    expect(css.slice(0, wideBlockStart)).toMatch(/\.sh-activity[\s\S]*?display:\s*none/)
    expect(css.slice(wideBlockStart)).toMatch(/\.sh-activity\s*\{[\s\S]*?display:\s*flex/)
  })

  it('builds its media query from the constant', () => {
    expect(LG_MEDIA_QUERY).toBe(`(min-width: ${LG_MIN_WIDTH}px)`)
  })
})

describe('createWideSubscription', () => {
  function fakeWindow(matches: boolean) {
    const listeners = new Set<() => void>()
    const mql = {
      matches,
      addEventListener: (_: string, cb: () => void) => void listeners.add(cb),
      removeEventListener: (_: string, cb: () => void) => void listeners.delete(cb),
    }
    const matchMedia = vi.fn(() => mql)
    return { win: { matchMedia } as unknown as Window, mql, listeners }
  }

  it('asks matchMedia for the shell breakpoint and reports its answer', () => {
    const { win } = fakeWindow(true)
    const sub = createWideSubscription(win)
    expect(sub.isWide()).toBe(true)
    expect((win.matchMedia as unknown as ReturnType<typeof vi.fn>)).toHaveBeenCalledWith(
      LG_MEDIA_QUERY,
    )
  })

  it('notifies on change and detaches on unsubscribe', () => {
    const { win, listeners } = fakeWindow(false)
    const sub = createWideSubscription(win)
    const onChange = vi.fn()
    const off = sub.subscribe(onChange)
    expect(listeners.size).toBe(1)
    for (const l of listeners) l()
    expect(onChange).toHaveBeenCalledTimes(1)
    off()
    expect(listeners.size).toBe(0)
  })

  // The narrow form renders every pane the wide form does, one at a time, so a
  // wrong answer here costs layout and not access. The other default would put a
  // three-column shell on a phone.
  it('reports narrow when the host has no matchMedia at all', () => {
    const sub = createWideSubscription({} as unknown as Window)
    expect(sub.isWide()).toBe(false)
    expect(() => sub.subscribe(() => {})()).not.toThrow()
  })

  it('falls back to the deprecated addListener when that is all the browser has', () => {
    const listeners = new Set<() => void>()
    const win = {
      matchMedia: () => ({
        matches: true,
        addListener: (cb: () => void) => void listeners.add(cb),
        removeListener: (cb: () => void) => void listeners.delete(cb),
      }),
    } as unknown as Window
    const off = createWideSubscription(win).subscribe(() => {})
    expect(listeners.size).toBe(1)
    off()
    expect(listeners.size).toBe(0)
  })
})
