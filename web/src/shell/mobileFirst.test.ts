/// <reference types="vite/client" />
// tether#173 decision 5 says mobile-first, and tether#195's constraints make two
// parts of it literal. Neither has an invariant ID in
// docs/tether-ui-invariants.md — the doc was extracted from a desktop-only test
// suite — so they are gated here as their own rules rather than labelled with
// somebody else's.
//
// 🔴 The object under test is the SOURCE TREE, read through vite's raw glob rather
// than through a list of filenames. That is deliberate: a file added to this
// directory next month inherits both rules without anyone remembering to add it,
// which is the difference between a gate and a checklist. `import.meta.glob` is
// resolved by vite at transform time against the real directory, so it cannot go
// stale the way a hand-maintained array does.

import { describe, expect, it } from 'vitest'

const shellSources = import.meta.glob('./*.{ts,tsx,css}', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

/**
 * Strips comments before the rules are applied.
 *
 * 🔴 Narrow on purpose, because preprocessing is how a self-check comes to hide
 * the defect it is checking for. What is removed is exactly the two comment
 * syntaxes — `/* … *\/` and a `//` line comment — and nothing else: no
 * whitespace collapsing, no string removal, no minification. A `100vh` in a
 * DECLARATION still reaches the assertion, and the mutation proof for these rules
 * injects one to show that it does.
 *
 * It is needed because this file's own subject matter is the forbidden tokens:
 * shell.css's header explains why `100dvh` is used "never `100vh`", and on the
 * first run that sentence tripped the rule three times. A rule that cannot be
 * written down beside the code it governs is a rule that stops being written down.
 */
function stripComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^[ \t]*\/\/.*$/gm, '')
}

/** Test files describe the rules; the rules are about what ships. */
function production(): [string, string][] {
  return Object.entries(shellSources)
    .filter(([path]) => !/\.test\.tsx?$/.test(path))
    .map(([path, text]) => [path, stripComments(text)])
}

describe('mobile-first', () => {
  it('has production sources to check — an empty set would pass vacuously', () => {
    // The emptiness guard exists for the same reason ci.yml's does: a rule
    // asserted over nothing is green and blind, and "there are no files" and
    // "every file complies" must not be the same result.
    expect(production().length).toBeGreaterThan(0)
  })

  // On iOS Safari `100vh` is the viewport with the browser chrome EXPANDED, so a
  // `100vh` root is taller than the screen whenever the chrome is collapsed and
  // the bottom of the app sits underneath it. `dvh` tracks the chrome and the
  // software keyboard. The old SPA used `100vh`; web/src/AuthPage.tsx on this
  // branch is the reference for the replacement (`minHeight: '100dvh'`).
  it('never uses 100vh — the dynamic viewport unit is the whole point', () => {
    for (const [path, text] of production()) {
      // `dvh`/`svh`/`lvh` are allowed; only the static `vh` on a full-height
      // value is not. Matching the unit with a boundary keeps `100dvh` from
      // being read as a hit.
      const hits = [...text.matchAll(/\b\d+vh\b/g)].map(m => m[0])
      expect(hits, `${path} uses a static vh unit`).toEqual([])
    }
  })

  // A width floor wider than the narrowest phone turns the page into a horizontal
  // scroller, which is what makes a "responsive" layout unusable rather than
  // merely cramped. `min-width: 0` is the opposite — it is what lets a flex child
  // shrink below its content — so it is allowed and is used throughout shell.css.
  it('declares no width floor above zero', () => {
    for (const [path, text] of production()) {
      // `(?<!\()` excludes `(min-width: …)`, which is a media-query CONDITION and
      // not a floor on any element. Without it this rule fires on a query — the
      // false positive it produced on first run, against breakpoint.ts's
      // LG_MEDIA_QUERY template.
      const css = [...text.matchAll(/(?<!\()min-width:\s*([^;)]+);/g)].map(m => m[1]!.trim())
      const js = [...text.matchAll(/minWidth:\s*([^,\n}]+)/g)].map(m => m[1]!.trim())
      for (const value of [...css, ...js]) {
        expect(
          value === '0' || value === "'0'" || value === '0px',
          `${path} declares min-width: ${value}`,
        ).toBe(true)
      }
    }
  })

  it('the shell root is sized in dvh, so it tracks the visible viewport', () => {
    const css = shellSources['./shell.css']
    expect(css).toBeTypeOf('string')
    // The raw import is stubbed to '' unless vite.config.ts sets `test.css`, and
    // an empty string would make the rules above pass by seeing nothing. Asserted
    // rather than assumed, because that is the failure mode of every gate on this
    // list.
    expect(css!.length).toBeGreaterThan(0)
    expect(css).toMatch(/\.sh-root\s*\{[^}]*height:\s*100dvh/)
  })

  // Guards the stripper itself. If stripComments ever ate a declaration, the two
  // rules above would go quietly green on a file that violates them — the
  // preprocessing swallowing the very defect it was added to make expressible.
  it('the comment stripper removes comments and nothing else', () => {
    const sample = [
      '/* a comment mentioning 100vh */',
      '.x { height: 100vh; } /* trailing 100vh */',
      '// a line comment mentioning minWidth: 320',
      'const a = { minWidth: 320 }',
    ].join('\n')
    const stripped = stripComments(sample)
    expect(stripped).toContain('height: 100vh;')
    expect(stripped).toContain('minWidth: 320')
    expect(stripped).not.toContain('a comment mentioning')
    expect(stripped).not.toContain('trailing')
    expect(stripped).not.toContain('a line comment')
  })
})
