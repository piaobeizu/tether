import { describe, it, expect } from 'vitest'
import {
  clampRightWidth,
  defaultRightWidth,
  loadRightWidth,
  MIN_RIGHT,
  MAX_RIGHT,
  MIN_MID,
  DEFAULT_RIGHT,
  DEFAULT_RIGHT_SHARE,
  DEFAULT_LEFT,
} from './layout'

/**
 * The activity bar's width, measured from index.css `.dt-activity`.
 *
 * A LITERAL on purpose, and deliberately not imported: lib/layout stopped
 * exporting ACTIVITY_W in tether#102, but even while it did, an expectation
 * phrased in terms of that constant is immune to that constant's VALUE — set it
 * to 0 and every such expectation moves with it and still passes. Written out,
 * the assertions below fail if layout.ts stops charging exactly 48. This is the
 * third place 48 appears (index.css, layout.ts, here) and the only one that is
 * a guard rather than a copy.
 */
const ACTIVITY_BAR_PX = 48

/** What the middle pane is left with: the window, less the chrome to its left
 *  (the workspace tree plus the activity bar), less the right pane. Before
 *  divider chrome — see the note above the last describe block. */
const mid = (windowWidth: number, treeWidth: number, rightWidth: number) =>
  windowWidth - (treeWidth + ACTIVITY_BAR_PX) - rightWidth

describe('clampRightWidth', () => {
  it('honours the requested width when the window has room for it', () => {
    // 1920 - (240 + 48) - 700 = 932 for the middle: nothing to protect against.
    expect(clampRightWidth(700, 1920, 240)).toBe(700)
  })

  it('lets the right pane become the widest pane — the point of tether#69', () => {
    // The old MAX_RIGHT of 600 made this impossible at 1440.
    const w = clampRightWidth(MAX_RIGHT, 1920, 240)
    expect(w).toBeGreaterThan(mid(1920, 240, w))
  })

  it('never exceeds MAX_RIGHT', () => {
    expect(clampRightWidth(99999, 5000, 240)).toBe(MAX_RIGHT)
  })

  it('never goes below MIN_RIGHT', () => {
    expect(clampRightWidth(0, 1920, 240)).toBe(MIN_RIGHT)
    expect(clampRightWidth(-500, 1920, 240)).toBe(MIN_RIGHT)
  })

  // The invariant this module exists for — stated as the conditional it actually
  // is. When the window cannot fit the left chrome + MIN_RIGHT + MIN_MID at all
  // there is no width that satisfies both panes, so the contract splits:
  //
  //   room enough  -> the middle pane keeps at least MIN_MID
  //   not enough   -> the right pane holds MIN_RIGHT and the middle gives way
  //
  // The second branch is a deliberate choice, not a gap: at that point the
  // alternative is a right pane below MIN_RIGHT, which trades one unusable pane
  // for two. An earlier version of this test asserted the invariant
  // unconditionally and failed at window=900/tree=480 — worth keeping the shape
  // of that case below.
  it.each([
    [1280, 240],
    [1440, 240],
    [1024, 160],
    [900, 480], // over-constrained: 480 + 48 + 260 + 320 = 1108 > 900
    [1920, 480],
  ])('respects the width contract at window=%i tree=%i', (windowWidth, treeWidth) => {
    const fits = windowWidth - (treeWidth + ACTIVITY_BAR_PX) >= MIN_RIGHT + MIN_MID
    for (const desired of [0, 300, 600, 900, MAX_RIGHT, 99999]) {
      const right = clampRightWidth(desired, windowWidth, treeWidth)
      if (fits) {
        expect(mid(windowWidth, treeWidth, right)).toBeGreaterThanOrEqual(MIN_MID)
      } else {
        expect(right).toBe(MIN_RIGHT)
      }
    }
  })

  it('falls back to MIN_RIGHT when even that leaves no room, rather than shrinking further', () => {
    // 600 - (160 + 48) - MIN_MID(320) = 72 of room, less than MIN_RIGHT: the pane
    // the user works in wins over the one behind it.
    expect(clampRightWidth(500, 600, 160)).toBe(MIN_RIGHT)
  })

  it('ignores a bogus window measurement instead of computing from it', () => {
    // jsdom before layout, a hidden tab: fall back to the constant bounds.
    for (const bogus of [0, -1, NaN, Infinity]) {
      expect(clampRightWidth(700, bogus, 240)).toBe(700)
    }
  })

  // A negative tree width is a corrupt persisted value (App.tsx's `loadWidth`
  // returns `Number(localStorage.getItem(key))` unvalidated), not a signal that
  // the activity bar has gone away. Both halves are asserted: the equality alone
  // would ALSO hold under the other placement of the clamp,
  // `Math.max(0, treeWidth + ACTIVITY_W)`, which drops the bar along with the
  // tree — so the equality on its own does not distinguish the two.
  it('treats a negative tree width as zero without dropping the activity bar too', () => {
    expect(clampRightWidth(MAX_RIGHT, 900, -1000)).toBe(clampRightWidth(MAX_RIGHT, 900, 0))
    // 900 - 48 - 320 = 532 of room, and the request (MAX_RIGHT) exceeds it.
    // Dropping the bar as well would leave 580.
    expect(clampRightWidth(MAX_RIGHT, 900, -1000)).toBe(532)
  })
})

describe('defaultRightWidth', () => {
  it('takes its share of the space beside the chrome left of the middle', () => {
    expect(defaultRightWidth(1440, 240)).toBe(
      Math.round((1440 - (240 + ACTIVITY_BAR_PX)) * DEFAULT_RIGHT_SHARE),
    )
    expect(defaultRightWidth(1440, 240)).toBe(645)
  })

  it('falls back to the constant when the window cannot be measured', () => {
    for (const bogus of [0, -1, NaN, Infinity]) {
      expect(defaultRightWidth(bogus, 240)).toBe(DEFAULT_RIGHT)
    }
  })

  // Same pair of claims as clampRightWidth's negative case, and the same reason
  // for asserting the absolute value beside the equality.
  it('treats a negative tree width as zero without dropping the activity bar too', () => {
    expect(defaultRightWidth(1440, -1000)).toBe(defaultRightWidth(1440, 0))
    // round((1440 - 48) * 0.56) = round(779.52) = 780. Dropping the bar as well
    // would give round(1440 * 0.56) = round(806.4) = 806.
    expect(defaultRightWidth(1440, -1000)).toBe(780)
  })

  // The constant is only reachable through the bogus-window branch, but it is
  // still a default width, so it has to satisfy the same property as the
  // computed one on the viewport it is nominally sized for.
  it('the fallback constant would itself be the widest pane at 1440 with the default tree', () => {
    expect(DEFAULT_RIGHT).toBeGreaterThan(mid(1440, 240, DEFAULT_RIGHT))
  })
})

describe('loadRightWidth', () => {
  it('uses the viewport-derived default when nothing was persisted', () => {
    expect(loadRightWidth(null, 1920, 240)).toBe(defaultRightWidth(1920, 240))
  })

  it.each([['', 'empty'], ['abc', 'garbage'], ['0', 'zero'], ['-40', 'negative']])(
    'falls back to the default for a %s stored value (%s)',
    (stored) => {
      expect(loadRightWidth(stored, 1920, 240)).toBe(defaultRightWidth(1920, 240))
    },
  )

  // tether#71 — the point of the whole item. The right pane is the primary
  // column, so on a first visit it must be the WIDEST column, not merely a
  // usable one. Stated over a table because a single-viewport assertion is
  // exactly the bug being fixed: DEFAULT_RIGHT = 560 was "correct" at 1274px
  // and nowhere anyone actually works.
  it.each([1280, 1366, 1440, 1536, 1600, 1920])(
    'makes the right pane the widest pane on a first visit at %ipx',
    (windowWidth) => {
      const tree = 240
      const right = loadRightWidth(null, windowWidth, tree)
      expect(right).toBeGreaterThan(0) // never pass on an absent measurement
      expect(right).toBeGreaterThan(mid(windowWidth, tree, right))
      expect(mid(windowWidth, tree, right)).toBeGreaterThanOrEqual(MIN_MID)
    },
  )

  // The band has an upper edge, and it is MAX_RIGHT's doing rather than an
  // oversight: past roughly MAX_RIGHT/SHARE of available width the cap binds and
  // the middle pane is wider again. Asserted so the ceiling is a decision on
  // record — a 1300px chat column is not what "primary" should mean.
  it('lets MAX_RIGHT win on a very wide monitor, leaving the middle wider', () => {
    const right = loadRightWidth(null, 2560, 240)
    expect(right).toBe(MAX_RIGHT)
    expect(right).toBeLessThan(mid(2560, 240, right))
  })

  it('still clamps the default so the middle pane survives a narrow window', () => {
    // 900 - (240 + 48) = 612 beside the left chrome; the share would ask for
    // round(342.72) = 343 and the MIN_MID rule allows 612 - 320 = 292. The
    // guarantee outranks the preference.
    const right = loadRightWidth(null, 900, 240)
    expect(right).toBeLessThan(defaultRightWidth(900, 240))
    expect(mid(900, 240, right)).toBeGreaterThanOrEqual(MIN_MID)
    // The exact width, not just the two properties above. This is one of only
    // two places in the file where the MIN_MID clamp actually BINDS on a value
    // that came through loadRightWidth, and the properties alone do not pin it:
    // a loadRightWidth that charged the bar a second time on its way into
    // clampRightWidth returns MIN_RIGHT (260) here, which is still less than
    // 343 and still leaves the middle ≥ 320, so both properties hold and the
    // mutant lives. Measured — it survived the first pass of tether#102's
    // mutation battery, which is why this line exists.
    expect(right).toBe(292)
  })

  it('restores a persisted width unchanged when it still fits', () => {
    expect(loadRightWidth('700', 1920, 240)).toBe(700)
  })

  // The read-side guard. Without it, a width stored on a wide monitor comes back
  // on a narrow one and the middle pane is crushed on every load — clamping only
  // on write is the usual gap in persist-and-clamp code.
  it('clamps a width persisted on a wider window so the middle pane survives', () => {
    const stored = String(MAX_RIGHT) // legitimate on a 2560px monitor
    const restored = loadRightWidth(stored, 1280, 240)
    expect(restored).toBeLessThan(MAX_RIGHT)
    expect(mid(1280, 240, restored)).toBeGreaterThanOrEqual(MIN_MID)
    // The other binding case, pinned exactly for the same reason as the one
    // above: 1280 - (240 + 48) - 320 = 672 of room. Charging the bar twice
    // gives 624, which also satisfies both properties.
    expect(restored).toBe(672)
  })
})

// tether#102. Until this item the third argument was called `leftWidth` and
// meant "all the chrome left of the middle column" — a sum App.tsx had to build
// at each call site (`loadWidth(STORAGE_KEY_LEFT, DEFAULT_LEFT) + ACTIVITY_W` in
// the `rightW` initializer, `leftW + ACTIVITY_W` in `resizeRight`). Nothing
// observed whether it did. Dropping the addend at BOTH sites left the whole
// suite green (measured, tether#100 review). Dropping it at ONE site — the
// initializer, leaving `resizeRight` intact so the import stays used and
// `tsc -b` still exits 0 — also left the whole suite green (measured at the head
// of tether#102: 30 files / 598 passed / 2 skipped). The one-site case is the
// realistic regression, and it was the one nothing could see: the two-site case
// is caught by `noUnusedLocals` before any test runs.
//
// The argument is now the TREE width and layout.ts adds the bar itself
// (chromeLeftOfMiddle), so:
//
//   · a call site cannot omit the bar — there is no addend to omit;
//   · a call site cannot double it either — ACTIVITY_W is no longer exported,
//     so `+ ACTIVITY_W` in App.tsx is TS2304 rather than a quiet 336.
//
// Neither of those is checked below; they are properties of the module's SHAPE
// and the compiler checks them. What is left for tests is the arithmetic: that
// the bar is charged, charged ONCE, and charged on the correct side of the
// negative-input clamp. Those are the forms in which the bug is still
// expressible, and all of them now live inside this module.
//
// Expected values are derived from the constants, NOT read off a run:
//
//   chrome = max(0, tree) + 48
//   beside = window - chrome
//   right  = round(beside * DEFAULT_RIGHT_SHARE)   [where neither clamp binds]
//   middle = beside - right
//
//   1440 -> chrome 288, beside 1152, right round(645.12) = 645, middle 507
//   1280 -> chrome 288, beside  992, right round(555.52) = 556, middle 436
//
// Out of scope on purpose and unchanged by tether#102: 505 and 434, the widths
// ForceGraph actually measures. Those are 507/436 less the two 1px
// `.col-resizer` dividers, and no function in layout.ts reads that 2px — see
// chromeLeftOfMiddle's note for why folding it in was rejected, on grounds
// other than "this module may not hold a stylesheet literal" (ACTIVITY_W is
// one). Asserting 505 here would be a guard that cannot fail for the reason it
// names. To be clear about what that costs: NO test anywhere asserts 505 or
// 434. ForceGraph.test.tsx derives them in prose, which is a different thing.
describe('the activity bar is charged inside layout.ts (tether#102)', () => {
  // The bar's contribution pinned per tree width. clampRightWidth's room
  // arithmetic is exact — no rounding anywhere in it — so where the MIN_MID
  // floor binds, the width it returns names the chrome directly:
  // right = window - chrome - MIN_MID. Window fixed at 1200 so every row has a
  // different expectation and has to be read rather than pattern-matched.
  it.each([
    [-1000, 48, 832], // corrupt persisted tree width: no tree, bar still there
    [0, 48, 832], // tree fully collapsed
    [160, 208, 672], // App.tsx MIN_LEFT
    [DEFAULT_LEFT, 288, 592], // the default — and what App.tsx passes
    [480, 528, 352], // App.tsx MAX_LEFT
  ])('charges a %ipx tree as %ipx of chrome', (treeWidth, chrome, expected) => {
    expect(1200 - chrome - MIN_MID).toBe(expected) // the row is self-consistent
    expect(clampRightWidth(99999, 1200, treeWidth)).toBe(expected)
  })

  it.each([
    [1440, 645, 507],
    [1280, 556, 436],
  ])(
    'gives the right pane %ipx -> %i and the middle %i on a first visit',
    (windowWidth, right, middle) => {
      // The whole chain App.tsx runs with nothing persisted, at the argument
      // App.tsx now actually passes.
      expect(loadRightWidth(null, windowWidth, DEFAULT_LEFT)).toBe(right)
      // Same number unclamped: neither MIN/MAX_RIGHT nor the MIN_MID rule binds
      // at these viewports, which is why the share alone predicts the result.
      expect(defaultRightWidth(windowWidth, DEFAULT_LEFT)).toBe(right)
      // What the middle column is left with, before divider chrome — the
      // quantity DEFAULT_RIGHT_SHARE's own note in layout.ts claims is 507/436.
      expect(mid(windowWidth, DEFAULT_LEFT, right)).toBe(middle)
      // tether#71's property, restated on the shape production uses.
      expect(right).toBeGreaterThan(middle)
      expect(middle).toBeGreaterThanOrEqual(MIN_MID)
    },
  )

  // 672 was `defaultRightWidth(1440, 240)` before tether#102 — the answer a
  // caller got by passing the tree alone, and the number tether#99's wrong
  // widths were built on. The same call now returns 645, because 240 IS the tree
  // and the bar is added regardless. The gap is 27, not 48: what the share loses
  // is 0.56 of the bar (26.88, twice rounded), not the bar.
  it('no longer returns the bar-forgotten width for the default tree', () => {
    const barForgotten = Math.round((1440 - DEFAULT_LEFT) * DEFAULT_RIGHT_SHARE)
    expect(barForgotten).toBe(672)
    expect(defaultRightWidth(1440, DEFAULT_LEFT)).toBe(645)
    expect(barForgotten - defaultRightWidth(1440, DEFAULT_LEFT)).toBe(27)
  })

  // The drag path (App.tsx `resizeRight`). Before tether#102 this block also
  // asserted what a bar-forgetting caller would be permitted — 440 at this
  // window against 392 — by calling the function with the tree alone. That call
  // no longer means anything different, so the claim is restated as what it was
  // always about: the floor is held exactly, and the too-loose width is not what
  // comes back.
  it('holds the MIN_MID floor exactly on the drag path', () => {
    const dragged = clampRightWidth(99999, 1000, DEFAULT_LEFT)
    expect(dragged).toBe(392)
    expect(mid(1000, DEFAULT_LEFT, dragged)).toBe(MIN_MID)
    // 1000 - 240 - 320 = 440: what the floor permits when the bar is not
    // charged, which is a middle of 272 against a floor of 320.
    expect(dragged).not.toBe(440)
    expect(440 - dragged).toBe(ACTIVITY_BAR_PX)
  })
})
