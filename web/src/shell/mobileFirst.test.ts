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

// Every stylesheet TETHER writes, by WILDCARD rather than by filename — so a file
// added to either directory next month inherits the rule below instead of having
// to be added to a list, which is this file's own stated difference between a gate
// and a checklist. Same glob shape as breakpoint.test.ts's, for the same reason
// its header records: scoped to `./*.css` alone, its width rule was green and
// blind to web/src/ui/index.css, the file that PR was editing.
//
// Deliberately NOT `cssLayers` above: that one names `../ui/index.css` exactly, so
// a second wrap stylesheet would escape it. It is left as it is because its two
// callers want that one file and nothing else.
//
// The vendored sheet is out of the set on purpose. It is upstream's, may not be
// edited, and it carries hover rules of its own — two `(hover: none) and
// (pointer: coarse)` blocks at this pin — so gating it would assert something
// about upstream that tether cannot act on.
const tetherStylesheets = import.meta.glob(['./*.css', '../ui/*.css'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

/** A `@media` prelude asking for a real hover capability. */
const HOVER_QUERY = /@media[^{]*\(\s*hover\s*:\s*hover\s*\)/

/**
 * The stylesheet with every `@media (hover: hover)` block removed — which is
 * exactly what a device with NO hover applies.
 *
 * Brace-MATCHED rather than regex-delimited, because a media block contains rule
 * blocks: the obvious `/@media[^{]*\{[^}]*\}/` stops at the first INNER `}` and
 * leaves the rest of the block's declarations behind, so the rules below would
 * read declarations a touch device never applies and fail on a correct file.
 * Guarded by its own case at the bottom of this file, for the reason the comment
 * stripper is: a stripper that ate too much turns every rule built on it green.
 */
function stripHoverQueries(css: string): string {
  let out = css
  for (;;) {
    const at = out.search(HOVER_QUERY)
    if (at < 0) return out
    const open = out.indexOf('{', at)
    if (open < 0) return out
    let depth = 0
    let end = open
    for (; end < out.length; end++) {
      if (out[end] === '{') depth++
      else if (out[end] === '}' && --depth === 0) break
    }
    out = out.slice(0, at) + out.slice(end + 1)
  }
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

  // 🔴 A touch device has no hover, and no `:focus-visible` before the tap. So a
  // control that is `opacity: 0` until `:hover` is, on the form this phase is
  // primarily for, a fully transparent target — present in the DOM, invisible on
  // screen. That is what `.sh-tree-hide` was: the one real control in the one real
  // pane, at 24px and opacity 0, on a workspace whose root is dominated by
  // generated sibling directories. It defeats owner ruling ③ directly, since that
  // ruling made the tree real *because acceptance is judged by the owner's eyes*.
  //
  // Two rules, both read off the whole stylesheet rather than off one selector, so
  // the next hover-revealed control inherits them:
  //
  //   1. nothing outside a `(hover: hover)` block may be `opacity: 0`
  //   2. no `:hover` selector outside one may touch visibility
  //
  // Rule 2 is what stops the obvious way around rule 1 — leaving the base rule
  // visible and hiding it from a `:hover` rule instead. `:hover` changing a
  // BACKGROUND outside the query is fine and is used throughout (`.sh-ws-row`,
  // `.sh-tree-line`, `.sh-resizer`): a device with no hover simply never gets the
  // highlight, which costs nothing.
  //
  // Both are stated as prohibitions rather than as "the reveal exists inside the
  // query", deliberately: deleting the hover-reveal outright and leaving the
  // control always visible is a perfectly good answer, and a gate that forbade it
  // would be pinning the decoration instead of the reachability.
  //
  // Check, all three run: (a) move `opacity: 0` out of the `@media (hover: hover)`
  // block in shell.css and into the base `.sh-tree-hide` rule — rule 1 fails; (b)
  // move the `:hover` reveal rule out of the block and leave `opacity: 0` inside —
  // rule 1 still passes and rule 2 fails; (c) put either violation in
  // web/src/ui/index.css instead — it fails there too, which is what the widened
  // glob buys.
  it('never hides a control behind hover alone — a touch device has no hover', () => {
    const sheets = Object.entries(tetherStylesheets)
    // Both halves of the glob have to have resolved. One guard over the union
    // would be satisfied by shell.css alone, which is the exact state
    // breakpoint.test.ts's width rule was in: green, and blind to the wrap layer.
    expect(
      sheets.filter(([p]) => p.startsWith('./')).length,
      'the shell glob resolved to nothing',
    ).toBeGreaterThan(0)
    expect(
      sheets.filter(([p]) => p.includes('/ui/')).length,
      'the wrap-layer glob resolved to nothing',
    ).toBeGreaterThan(0)

    for (const [path, raw] of sheets) {
      // Non-empty on the RAW text: the import is stubbed to '' unless
      // vite.config.ts sets `test.css`, and a stripped '' is also '' — so
      // checking after the strip could not tell "nothing was read" from
      // "everything was a comment". Same phrasing as breakpoint.test.ts's.
      expect(raw.length, `${path} read back empty — the raw import is stubbed`).toBeGreaterThan(0)

      // Everything a browser with NO hover applies: the sheet with its comments
      // gone (this file's header quotes the patterns below in prose) and every
      // `(hover: hover)` block removed, brace-matched from the query's own `{`.
      const base = stripHoverQueries(stripComments(raw))

      const transparent = [...base.matchAll(/opacity:\s*0(?![.\d])/g)].map(m => m[0])
      expect(
        transparent,
        `${path} sets opacity: 0 outside a (hover: hover) query — invisible on touch`,
      ).toEqual([])

      for (const [, selector, body] of base.matchAll(/([^{}]*:hover[^{}]*)\{([^}]*)\}/g)) {
        expect(
          /(?:opacity|visibility|display)\s*:/.test(body!),
          `${path}: \`${selector!.trim()}\` changes visibility outside a (hover: hover) query`,
        ).toBe(false)
      }
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

  // Guards the OTHER stripper, for the same reason and against both directions of
  // error. Too little and the hover rule fails on a correct file; too much and it
  // passes on a violating one, which is the direction that ships.
  it('the hover-query stripper removes the whole block and nothing outside it', () => {
    const sample = [
      '.a { opacity: 0 }',
      '@media (hover: hover) {',
      '  .b { opacity: 0 }',
      '  .c:hover .b { opacity: 1 }',
      '}',
      '.d:hover { background: red }',
    ].join('\n')
    const base = stripHoverQueries(sample)

    // The block goes, INCLUDING its nested rules. A regex delimited by the first
    // `}` would leave `.c:hover .b { opacity: 1 }` standing, and the rule above
    // would then fail on a stylesheet that is correct.
    expect(base).not.toContain('.b {')
    expect(base).not.toContain('.c:hover')
    // …and nothing outside it moves. This half is the one that matters: a
    // stripper that ate the file would turn the rule above green on exactly the
    // stylesheet it was written to reject — `.a { opacity: 0 }` is that
    // stylesheet, and it has to survive to be seen.
    expect(base).toContain('.a { opacity: 0 }')
    expect(base).toContain('.d:hover { background: red }')
  })
})
