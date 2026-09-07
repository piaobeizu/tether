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
import { stripCssComments as stripComments } from './cssText'

const shellSources = import.meta.glob('./*.{ts,tsx,css}', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

// The wrap layer and the pristine upstream stylesheet it consumes. Both are read
// because the rules below are about the CASCADE, not about one file: the shell's
// own root can be correct while an ancestor styled by the vendored sheet is not.
const cssLayers = import.meta.glob(['../ui/index.css', '../vendor/**/index.css'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

// The comment stripper used to be defined here. It now lives in ./cssText.ts,
// because breakpoint.test.ts needs the same one — its own rules were tripped by
// shell.css's header quoting the very `@media (min-width: …)` pattern it bans —
// and two copies of a stripper is two things that can drift apart, in the one
// place where drift is invisible. The guard test at the bottom of this file moved
// with the responsibility: it now guards every caller rather than this file.

/**
 * The declaration block a stylesheet opens for exactly `selector`, or null.
 *
 * "Exactly" is the whole job. The selector has to sit at a rule boundary — start
 * of a line, or after a `}` or `;` — so that looking for `#root` does not find the
 * `#root` inside `body.pwa-mode #root`. That substring match is what let the
 * first version of the override check pass on a tree with the override deleted.
 */
function ruleFor(css: string, selector: string): string | null {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const m = new RegExp(`(?:^|[};])\\s*${escaped}\\s*\\{([^}]*)\\}`, 'm').exec(css)
  return m === null ? null : m[1]!
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

  // 🔴 The rule above is about files this wi writes, and it was NOT sufficient:
  // `.sh-root` was already `100dvh` and the built bundle still carried a `100vh`,
  // because the vendored stylesheet pins `#root` — the shell's own parent — at the
  // static unit. A rule scoped to "my files" cannot see that, so this is the same
  // rule scoped to the cascade.
  //
  // The selector list is DERIVED from the vendored file rather than written out,
  // which is what makes it hold for a pin upstream adds at the next tag. The
  // vendored file cannot be edited (its body and header are hashed both ways), so
  // the answer is an override in the wrap layer.
  it('overrides every static-vh rule the vendored stylesheet applies', () => {
    const vendorPath = Object.keys(cssLayers).find(p => p.includes('/vendor/'))
    expect(vendorPath, 'the vendored stylesheet was not readable').toBeTypeOf('string')
    const vendor = stripComments(cssLayers[vendorPath!]!)
    expect(vendor.length).toBeGreaterThan(0)

    const shell = stripComments(shellSources['./shell.css']!)

    // Every selector block in the vendored sheet holding a static vh value.
    const pinned = [...vendor.matchAll(/([^{}]+)\{([^}]*\b\d+vh\b[^}]*)\}/g)].map(m =>
      m[1]!.trim().split('\n').pop()!.trim(),
    )
    expect(pinned.length, 'no vendored vh rule found — has the pin moved?').toBeGreaterThan(0)

    for (const selector of pinned) {
      // 🔴 A `toContain(selector)` here is NOT enough, and this is not a
      // hypothetical: it was the first version, and deleting the whole
      // `#root { min-height: 100dvh }` block left it GREEN — because
      // `body.pwa-mode #root` still contains the substring `#root`. A selector
      // has to be matched at a rule boundary, and the block it opens has to
      // actually carry a dynamic unit, or this checks that a name appears
      // somewhere rather than that a rule overrides anything.
      const rule = ruleFor(shell, selector)
      expect(rule, `nothing overrides the vendored \`${selector}\` vh rule`).not.toBeNull()
      expect(rule!, `the \`${selector}\` override carries no dvh value`).toMatch(/\b\d+dvh\b/)
      expect(rule!, `the \`${selector}\` override still uses a static vh`).not.toMatch(/\b\d+vh\b/)
    }
  })

  // The override only wins because of import order, so the order is asserted
  // rather than assumed. Reversing the two imports would leave every rule above
  // green and the shipped page wrong.
  it('imports the wrap layer after the vendored sheet, so overrides win', () => {
    const wrap = Object.entries(cssLayers).find(([p]) => p.includes('/ui/'))
    expect(wrap, 'the wrap stylesheet was not readable').toBeTruthy()
    const text = wrap![1]
    const vendorAt = text.indexOf("@import '../vendor/")
    const shellAt = text.indexOf("@import '../shell/shell.css'")
    expect(vendorAt).toBeGreaterThan(-1)
    expect(shellAt).toBeGreaterThan(-1)
    expect(shellAt).toBeGreaterThan(vendorAt)
  })

  // Guards the stripper itself, for every gate that uses it — the rules above and
  // breakpoint.test.ts's width-media-query rule, which is why it lives in
  // ./cssText.ts. If it ever ate a declaration, all of them would go quietly green
  // on a file that violates them: the preprocessing swallowing the very defect it
  // was added to make expressible.
  it('the comment stripper removes comments and nothing else', () => {
    const sample = [
      '/* a comment mentioning 100vh and @media (min-width: 900px) */',
      '.x { height: 100vh; } /* trailing 100vh */',
      '// a line comment mentioning minWidth: 320',
      'const a = { minWidth: 320 }',
      '@media (min-width: 900px) { .y { color: red } }',
    ].join('\n')
    const stripped = stripComments(sample)
    expect(stripped).toContain('height: 100vh;')
    expect(stripped).toContain('minWidth: 320')
    // The token breakpoint.test.ts's rule is about, surviving in a real rule.
    expect(stripped).toContain('@media (min-width: 900px) { .y')
    expect(stripped).not.toContain('a comment mentioning')
    expect(stripped).not.toContain('trailing')
    expect(stripped).not.toContain('a line comment')
  })
})
