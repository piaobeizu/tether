/// <reference types="vite/client" />
// The shell's breakpoint is duplicated in three places that cannot import each
// other — a TypeScript constant, a CSS media query, and Tailwind's `lg` — so this
// file is what stops them drifting. Each assertion compares the copy against the
// authority, never two copies against each other.

import { describe, expect, it, vi } from 'vitest'
import defaultTheme from 'tailwindcss/defaultTheme'
import { createWideSubscription, LG_MEDIA_QUERY, LG_MIN_WIDTH } from './breakpoint'

// `?raw` through vite's glob, so the assertions read the files that ship rather
// than a fixture. A file added to this directory later is picked up without being
// added to a list.
const sources = import.meta.glob('./*.css', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

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

  // 🔴 The breakpoint exists ONCE, in TypeScript. A width media query in the shell
  // stylesheet would be a second copy of it, free to disagree — the layout
  // switching at one width while LAY-14's mounting switched at another, since the
  // two forms are different trees and `display` cannot express a mount. So the
  // assertion is not "the query matches the constant", it is "there is no query".
  //
  // Reading the shipped stylesheet rather than a fixture is what makes this hold
  // for a file added later: the glob is resolved against the real directory.
  it('is not duplicated into the stylesheet — the shell has no width media query', () => {
    expect(Object.keys(sources).length).toBeGreaterThan(0)
    for (const [path, css] of Object.entries(sources)) {
      expect(css.length, `${path} read back empty — the raw import is stubbed`).toBeGreaterThan(0)
      const widthQueries = [...css.matchAll(/@media[^{]*\((?:min|max)-width:[^)]*\)/g)].map(
        m => m[0],
      )
      expect(widthQueries, `${path} declares a width breakpoint`).toEqual([])
    }
  })

  // The switch the stylesheet does use has to be the one the shell publishes, or
  // the wide rules are dead code.
  it('drives the wide layout from the attribute the shell publishes', () => {
    const css = sources['./shell.css']!
    expect(css).toContain("[data-wide='true']")
  })

  // The narrow layout is what a browser applies before any JavaScript runs, so
  // "mobile-first" is a property of where the rules sit rather than a claim.
  it("keeps the wide rules behind the attribute, so the default cascade is the narrow form", () => {
    const css = sources['./shell.css']!
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
