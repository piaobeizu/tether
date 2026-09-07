/// <reference types="vite/client" />
// tether#173 decision 5 says mobile-first, and tether#195's constraints make two
// parts of it literal. Neither has an invariant ID in
// <workspace>/docs/tether-ui-invariants.md — the doc was extracted from a
// desktop-only test suite — so they are gated here as their own rules rather
// than labelled with somebody else's.
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

// ── what the hover rules below reason about ─────────────────────────────────
//
// 🔴 A stylesheet is parsed into RULES here rather than pattern-matched as text,
// and that is not tidiness. Every shape this file used before read the text NEAR
// a declaration instead of the rule that holds it, and each one failed in a way
// that was measured, not imagined:
//
//   * `/([^{}]*:hover[^{}]*)\{([^}]*)\}/` begins its selector capture at the
//     PREVIOUS brace. A violation in web/src/ui/index.css — whose only other
//     content is two `@import` lines — was reported with those two lines as the
//     offending selector. Right verdict, unusable message.
//   * the same shape's body capture stops at the first INNER `}`, so a
//     declaration in a nested rule is charged to the block around it. shell.css
//     nests: `[data-wide='true']` holds nine rules.
//   * `@media (hover: hover)` as a literal pinned ONE spelling of a capability
//     question. Rewriting shell.css's query as the equally correct
//     `@media (pointer: fine)` turned the rule red on a file with no defect in
//     it. A gate that fires on correct input is a gate people delete, so the
//     breadth below is part of it working rather than a relaxation of it.

/**
 * A declaration block: what opens it, what is written DIRECTLY inside it, and
 * the at-rule preludes it is nested in.
 */
type Rule = { selector: string; body: string; at: string[] }

/**
 * Every declaration block in `css`, brace-tracked.
 *
 * Guarded by its own case at the bottom of this file, for the reason the comment
 * stripper is: a parser that returned nothing — or that charged a declaration to
 * the wrong block — turns every rule built on it green on exactly the stylesheet
 * it was written to reject.
 */
function rules(css: string): Rule[] {
  const out: Rule[] = []
  const stack: Rule[] = []
  let pending = ''
  for (const ch of css) {
    if (ch === '{') {
      // The selector text just accumulated belongs to the block being opened,
      // not to the one enclosing it — without this, `[data-wide='true']`'s body
      // would carry the text of all nine selectors nested inside it.
      const parent = stack[stack.length - 1]
      if (parent !== undefined) {
        parent.body = parent.body.slice(0, parent.body.length - pending.length)
      }
      stack.push({
        selector: pending.trim(),
        body: '',
        at: stack.filter(f => f.selector.startsWith('@')).map(f => f.selector),
      })
      pending = ''
      continue
    }
    if (ch === '}') {
      const frame = stack.pop()
      if (frame !== undefined) out.push(frame)
      pending = ''
      continue
    }
    const top = stack[stack.length - 1]
    if (top !== undefined) top.body += ch
    pending = ch === ';' ? '' : pending + ch
  }
  return out
}

/**
 * A media condition asking for a hover-capable pointer.
 *
 * A SET of spellings, because they are all the same question put to the device.
 * `(hover: hover)` is what shell.css asks today; `(pointer: fine)`,
 * `(any-hover: hover)` and `(any-pointer: fine)` are the other ways to ask it,
 * and these rules care about the answer rather than the wording.
 */
const HOVER_CAPABLE = /\(\s*(?:any-)?hover\s*:\s*hover\s*\)|\(\s*(?:any-)?pointer\s*:\s*fine\s*\)/

/** `@keyframes`, whose blocks declare an animation timeline rather than the
 *  state any element is in. */
const KEYFRAMES = /^@(?:-\w+-)?keyframes\b/

/**
 * Whether a browser with NO hover skips this at-rule's block.
 *
 * EVERY comma branch has to ask for hover, because `@media A, B` applies when
 * either holds: a touch device applies `(hover: hover), (pointer: coarse)`
 * through the second branch, so that block's contents must stay in view.
 *
 * A `not` prelude is deliberately not read as hover-capable. `@media not
 * (hover: hover)` is TRUE on a touch device, so skipping it would be a hole in
 * the gate rather than a false positive on correct input — the direction that
 * ships. Nothing in tether writes that form; if something does, the fix is to
 * write the positive one.
 */
function needsHover(prelude: string): boolean {
  const m = /^@media\b([\s\S]*)$/.exec(prelude.trim())
  if (m === null) return false
  return m[1]!.split(',').every(branch => HOVER_CAPABLE.test(branch) && !/\bnot\b/.test(branch))
}

/**
 * The rules a device with no hover applies.
 *
 * `@keyframes` bodies are out, and that is what keeps this from firing on a
 * legitimate fade-in: a keyframe is a point on a timeline, not a state an
 * element is in, so `from { opacity: 0 }` hides nothing. Appending one plain
 * `@keyframes sh-fade-in { from { opacity: 0 } to { opacity: 1 } }` to shell.css
 * used to fail this file with "invisible on touch" — on correct input, and on a
 * near-term edit, since `.sh-drawer-panel` already ships a `transition` and a
 * `prefers-reduced-motion` block beside it.
 */
function baseRules(all: Rule[]): Rule[] {
  return all.filter(
    r => !r.selector.startsWith('@') && !r.at.some(a => needsHover(a) || KEYFRAMES.test(a)),
  )
}

/** The rules ONLY a hover-capable device applies. */
function hoverOnlyRules(all: Rule[]): Rule[] {
  return all.filter(r => !r.selector.startsWith('@') && r.at.some(a => needsHover(a)))
}

/** `(property, value)` for every visibility-affecting declaration in a body. */
function visibilityDecls(body: string): [string, string][] {
  return [...body.matchAll(/(opacity|visibility|display)\s*:\s*([^;}]+)/g)].map(m => [
    m[1]!,
    m[2]!.trim().replace(/\s*!important$/, ''),
  ])
}

/**
 * `value` read as an alpha, or null when it is not a literal number.
 *
 * 🔴 EVERY spelling of zero, not the one literal `0`. The rule below used to be
 * `/opacity:\s*0(?![.\d])/`, whose lookahead excludes `0.0` and whose anchor
 * never reaches `.0` — both of them fully transparent, and both measured GREEN
 * with the tree's one control invisible on touch.
 */
function alphaOf(value: string): number | null {
  if (!/^[-.\d]/.test(value)) return null
  const pct = /^(-?[\d.]+)%$/.exec(value)
  const n = pct === null ? Number(value) : Number(pct[1]!) / 100
  return Number.isFinite(n) ? n : null
}

/** Whether the declaration takes the element out of sight. */
function hides(prop: string, value: string): boolean {
  if (prop === 'opacity') {
    const alpha = alphaOf(value)
    return alpha !== null && alpha <= 0
  }
  if (prop === 'visibility') return value === 'hidden' || value === 'collapse'
  return prop === 'display' && value === 'none'
}

/** …and whether it brings the element back. */
function reveals(prop: string, value: string): boolean {
  if (prop === 'opacity') {
    const alpha = alphaOf(value)
    return alpha !== null && alpha > 0
  }
  if (prop === 'visibility') return value === 'visible'
  return prop === 'display' && value !== 'none'
}

/**
 * The element each comma branch of a selector STYLES, as a bare key.
 *
 * `.sh-tree-line:hover .sh-tree-hide` styles `.sh-tree-hide`, not
 * `.sh-tree-line`, so the subject is the LAST combinator-separated compound with
 * its pseudo-classes stripped. Ancestor context is dropped, which makes the key
 * coarser than a real selector match — `[data-wide='true'] .sh-nav-toggle` and a
 * bare `.sh-nav-toggle` share it. Rule 3 is phrased so that coarseness cannot
 * produce a false positive by itself: it needs the hover block to actually
 * REVEAL the subject, not merely to mention it.
 */
function subjectsOf(selector: string): string[] {
  return selector
    .split(',')
    .map(sel => (sel.trim().split(/[\s>+~]+/).pop() ?? '').replace(/::?[\w-]+(?:\([^)]*\))?/g, ''))
    .filter(key => key.length > 0)
}

/**
 * Every stylesheet tether writes, comment-stripped and parsed, with the
 * emptiness guards each of the three hover rules needs.
 *
 * The guards are here rather than in one of the rules because "nothing was read"
 * and "everything complies" must not be the same result, and each rule below is
 * a loop that is silently green over an empty set.
 */
function tetherRules(): [string, Rule[]][] {
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

  const walked: [string, Rule[]][] = []
  for (const [path, raw] of sheets) {
    // Non-empty on the RAW text: the import is stubbed to '' unless
    // vite.config.ts sets `test.css`, and a stripped '' is also '' — so checking
    // after the strip could not tell "nothing was read" from "everything was a
    // comment". Same phrasing as breakpoint.test.ts's.
    expect(raw.length, `${path} read back empty — the raw import is stubbed`).toBeGreaterThan(0)
    // Comments go first because this file's own header, and shell.css's, quote
    // the patterns below in prose.
    walked.push([path, rules(stripComments(raw))])
  }
  // …and the parse has to have found rules. web/src/ui/index.css legitimately
  // holds none (it is two `@import`s), so this is asserted over the union.
  expect(
    walked.flatMap(([, parsed]) => parsed).length,
    'no rule parsed out of any stylesheet',
  ).toBeGreaterThan(0)
  return walked
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
  // control that is invisible until `:hover` is, on the form this phase is
  // primarily for, a fully transparent target — present in the DOM, invisible on
  // screen. That is what `.sh-tree-hide` was: the one real control in the one real
  // pane, at 24px and opacity 0, on a workspace whose root is dominated by
  // generated sibling directories. It defeats owner ruling ③ directly, since that
  // ruling made the tree real *because acceptance is judged by the owner's eyes*.
  //
  // Three rules, all read off the whole stylesheet rather than off one selector,
  // so the next hover-revealed control inherits them:
  //
  //   1. the base cascade may not hide an element by opacity or visibility
  //   2. no `:hover` selector outside a hover query may touch visibility
  //   3. a hover query may not REVEAL what the base cascade hid
  //
  // Rule 2 is what stops the obvious way around rule 1 — leaving the base rule
  // visible and hiding the element from a `:hover` rule instead. `:hover` changing
  // a BACKGROUND outside the query is fine and is used throughout (`.sh-ws-row`,
  // `.sh-tree-line`, `.sh-resizer`): a device with no hover simply never gets the
  // highlight, which costs nothing.
  //
  // 🔴 Rule 3 exists because rule 1 CANNOT be widened to `display`, and the
  // reason is worth stating: `display: none` is the ordinary way to hide
  // something that should not show, and shell.css uses it that way — LAY-14's
  // mounted-but-not-showing pane, and the three pieces of wide-form chrome the
  // drawer replaces. A blanket ban would fire on all of them, i.e. on correct
  // code, and that is the same defect as a hole: false positives are what get a
  // gate deleted. So the `display` form of this defect is caught by the shape
  // that is specific to it — base cascade hides the element, hover query brings
  // it back — which no legitimate rule in this stylesheet has.
  //
  // All three are prohibitions rather than "the reveal exists inside the query",
  // deliberately: deleting the hover-reveal outright and leaving the control
  // always visible is a perfectly good answer, and a gate that forbade it would
  // be pinning the decoration instead of the reachability.
  it('rule 1: the base cascade hides no element — a touch device has no hover', () => {
    for (const [path, all] of tetherRules()) {
      for (const rule of baseRules(all)) {
        const hidden = visibilityDecls(rule.body)
          .filter(([prop, value]) => prop !== 'display' && hides(prop, value))
          .map(([prop, value]) => `${prop}: ${value}`)
        expect(
          hidden,
          `${path}: \`${rule.selector}\` hides the element outside a hover-capable query — invisible on touch`,
        ).toEqual([])
      }
    }
  })

  it('rule 2: no `:hover` rule outside a hover-capable query touches visibility', () => {
    for (const [path, all] of tetherRules()) {
      for (const rule of baseRules(all)) {
        if (!rule.selector.includes(':hover')) continue
        expect(
          visibilityDecls(rule.body).map(([prop]) => prop),
          `${path}: \`${rule.selector}\` changes visibility outside a hover-capable query`,
        ).toEqual([])
      }
    }
  })

  it('rule 3: a hover-capable query reveals nothing the base cascade hid', () => {
    for (const [path, all] of tetherRules()) {
      const hiddenBy = new Map<string, string>()
      for (const rule of baseRules(all)) {
        for (const [prop, value] of visibilityDecls(rule.body)) {
          if (!hides(prop, value)) continue
          for (const key of subjectsOf(rule.selector)) hiddenBy.set(key, `${prop}: ${value}`)
        }
      }
      for (const rule of hoverOnlyRules(all)) {
        if (!visibilityDecls(rule.body).some(([prop, value]) => reveals(prop, value))) continue
        for (const key of subjectsOf(rule.selector)) {
          expect(
            hiddenBy.get(key),
            `${path}: \`${rule.selector}\` reveals \`${key}\`, which the base cascade hides — invisible on touch`,
          ).toBeUndefined()
        }
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

  // Guards the PARSER, for the same reason and against both directions of error.
  // Too little and the hover rules fail on a correct file; too much and they pass
  // on a violating one, which is the direction that ships.
  it('the rule parser charges each declaration to the block that holds it', () => {
    const sample = [
      "@import 'a.css';",
      '.a { opacity: 0 }',
      '@media (hover: hover) {',
      '  .b { opacity: 0 }',
      '  .c:hover .b { opacity: 1 }',
      '}',
      "[data-wide='true'] {",
      '  .d { display: none }',
      '}',
      '.e:hover { background: red }',
      '@keyframes fade { from { opacity: 0 } to { opacity: 1 } }',
    ].join('\n')
    const all = rules(sample)
    const body = (selector: string) => all.find(r => r.selector === selector)!.body

    // What a hoverless device applies, in order of closing brace: the hover
    // block's two rules are gone, the keyframe steps are gone, and the NESTED
    // rule and its parent are both present and distinct. The at-rules are not
    // rules and are not here.
    expect(baseRules(all).map(r => r.selector)).toEqual([
      '.a',
      '.d',
      "[data-wide='true']",
      '.e:hover',
    ])
    // The selector is the rule's OWN, not the text back to the previous brace —
    // which is what made a violation in web/src/ui/index.css report that file's
    // two `@import` lines as its selector.
    expect(hoverOnlyRules(all).map(r => r.selector)).toEqual(['.b', '.c:hover .b'])

    // The half that matters most: a declaration outside the hover block
    // SURVIVES. A parser that ate it would turn rule 1 green on exactly the
    // stylesheet it was written to reject.
    expect(visibilityDecls(body('.a'))).toEqual([['opacity', '0']])
    // …and a nested declaration is not charged to the block around it, which is
    // what the regex shape got wrong.
    expect(visibilityDecls(body('.d'))).toEqual([['display', 'none']])
    expect(visibilityDecls(body("[data-wide='true']"))).toEqual([])
  })

  // Guards the capability predicate in both directions too, because both are
  // load-bearing: a spelling it fails to recognise makes the rules above fire on
  // a correct file, and one it recognises too eagerly makes them blind.
  it('reads every spelling of "this device has hover", and only those', () => {
    for (const prelude of [
      '@media (hover: hover)',
      '@media(hover:hover)', // what the minifier emits
      '@media (any-hover: hover)',
      '@media (pointer: fine)',
      '@media (any-pointer: fine)',
      '@media (min-width: 900px) and (hover: hover)',
      '@media (hover: hover), (pointer: fine)',
    ]) {
      expect(needsHover(prelude), `${prelude} asks for a hover-capable pointer`).toBe(true)
    }

    for (const prelude of [
      '@media (hover: none)',
      '@media (pointer: coarse)',
      '@media (prefers-reduced-motion: reduce)',
      '@media (min-width: 900px)',
      // applied by a touch device through its SECOND branch
      '@media (hover: hover), (pointer: coarse)',
      // TRUE on a touch device, so left in view of the rules rather than skipped
      '@media not (hover: hover)',
      '@keyframes fade',
      "[data-wide='true']",
    ]) {
      expect(needsHover(prelude), `${prelude} does not`).toBe(false)
    }
  })

  // The zero the opacity rule is about, in every spelling a stylesheet can write
  // it. `0.0` and `.0` are the two that shipped past the old
  // `/opacity:\s*0(?![.\d])/` form — measured green with the control invisible —
  // so they are pinned here rather than left to the mutation proof alone.
  it('reads every spelling of a transparent opacity, and only those', () => {
    for (const value of ['0', '0.0', '.0', '0%', '0.00', '-0'])
      expect(hides('opacity', value), `opacity: ${value}`).toBe(true)
    for (const value of ['1', '0.5', '.5', '50%', 'var(--x)', 'inherit'])
      expect(hides('opacity', value), `opacity: ${value}`).toBe(false)
    // `!important` is stripped by the reader rather than handled by every
    // predicate, so it is pinned where it is stripped.
    expect(visibilityDecls('opacity: 0 !important;')).toEqual([['opacity', '0']])
  })
})
