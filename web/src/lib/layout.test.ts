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
  ACTIVITY_W,
} from './layout'

/** What the middle pane is left with for a given window/left/right. */
const mid = (windowWidth: number, leftWidth: number, rightWidth: number) =>
  windowWidth - leftWidth - rightWidth

describe('clampRightWidth', () => {
  it('honours the requested width when the window has room for it', () => {
    // 1920 - 240 - 700 = 980 for the middle: nothing to protect against.
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
  // is. When the window cannot fit leftWidth + MIN_RIGHT + MIN_MID at all there
  // is no width that satisfies both panes, so the contract splits:
  //
  //   room enough  -> the middle pane keeps at least MIN_MID
  //   not enough   -> the right pane holds MIN_RIGHT and the middle gives way
  //
  // The second branch is a deliberate choice, not a gap: at that point the
  // alternative is a right pane below MIN_RIGHT, which trades one unusable pane
  // for two. An earlier version of this test asserted the invariant
  // unconditionally and failed at window=900/left=480 — worth keeping the shape
  // of that case below.
  it.each([
    [1280, 240],
    [1440, 240],
    [1024, 160],
    [900, 480], // over-constrained: 480 + 260 + 320 = 1060 > 900
    [1920, 480],
  ])('respects the width contract at window=%i left=%i', (windowWidth, leftWidth) => {
    const fits = windowWidth - leftWidth >= MIN_RIGHT + MIN_MID
    for (const desired of [0, 300, 600, 900, MAX_RIGHT, 99999]) {
      const right = clampRightWidth(desired, windowWidth, leftWidth)
      if (fits) {
        expect(mid(windowWidth, leftWidth, right)).toBeGreaterThanOrEqual(MIN_MID)
      } else {
        expect(right).toBe(MIN_RIGHT)
      }
    }
  })

  it('falls back to MIN_RIGHT when even that leaves no room, rather than shrinking further', () => {
    // 600 - 160 - MIN_MID(320) = 120 of room, less than MIN_RIGHT: the pane the
    // user works in wins over the one behind it.
    expect(clampRightWidth(500, 600, 160)).toBe(MIN_RIGHT)
  })

  it('ignores a bogus window measurement instead of computing from it', () => {
    // jsdom before layout, a hidden tab: fall back to the constant bounds.
    for (const bogus of [0, -1, NaN, Infinity]) {
      expect(clampRightWidth(700, bogus, 240)).toBe(700)
    }
  })

  it('treats a negative left width as zero rather than widening the right pane', () => {
    expect(clampRightWidth(MAX_RIGHT, 900, -1000)).toBe(clampRightWidth(MAX_RIGHT, 900, 0))
  })
})

describe('defaultRightWidth', () => {
  // NOTE the leftWidth: 240 is the TREE alone, which is not what production
  // passes — see the 'production parameter shape' block at the bottom of this
  // file. 672 is a fact about this function, not the width anyone gets.
  it('takes its share of the space beside the left pane', () => {
    expect(defaultRightWidth(1440, 240)).toBe(Math.round(1200 * DEFAULT_RIGHT_SHARE))
    expect(defaultRightWidth(1440, 240)).toBe(672)
  })

  it('falls back to the constant when the window cannot be measured', () => {
    for (const bogus of [0, -1, NaN, Infinity]) {
      expect(defaultRightWidth(bogus, 240)).toBe(DEFAULT_RIGHT)
    }
  })

  it('treats a negative left width as zero rather than widening the right pane', () => {
    expect(defaultRightWidth(1440, -1000)).toBe(defaultRightWidth(1440, 0))
  })

  // The constant is only reachable through the bogus-window branch, but it is
  // still a default width, so it has to satisfy the same property as the
  // computed one on the viewport it is nominally sized for.
  it('the fallback constant would itself be the widest pane at 1440/240', () => {
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
      const left = 240
      const right = loadRightWidth(null, windowWidth, left)
      expect(right).toBeGreaterThan(0) // never pass on an absent measurement
      expect(right).toBeGreaterThan(mid(windowWidth, left, right))
      expect(mid(windowWidth, left, right)).toBeGreaterThanOrEqual(MIN_MID)
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
    // 900 - 240 = 660 beside the left pane; the share would ask for 370 and the
    // MIN_MID rule allows 340. The guarantee outranks the preference.
    const right = loadRightWidth(null, 900, 240)
    expect(right).toBeLessThan(defaultRightWidth(900, 240))
    expect(mid(900, 240, right)).toBeGreaterThanOrEqual(MIN_MID)
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
  })
})

// tether#100. Every leftWidth in the blocks above is a bare number chosen to
// exercise a rule — 240 mostly, also 160, 480, 0 and -1000. None of them is what
// production passes. App.tsx's `rightW` initializer hands loadRightWidth
// `loadWidth(STORAGE_KEY_LEFT, DEFAULT_LEFT) + ACTIVITY_W`, and the divider drag
// hands clampRightWidth `leftW + ACTIVITY_W`, because both rules here are
// arithmetic on what is left for the MIDDLE column and the chrome to its left is
// the activity bar plus the tree. So the number this module is actually asked
// about on a first visit is 288, and until tether#100 no test named it: the two
// addends lived in different modules (ACTIVITY_W was private to App.tsx), so the
// sum could not be written down here at all.
//
// That is the gap tether#99's two wrong widths came through. Re-derived here
// rather than copied — the wi that filed this one reports the slip as
// `1440 - 240 - round((1440 - 240) * 0.56)`, but that is 528, not 478, and it
// contains no ACTIVITY_W term at all, so it cannot be a mistake about
// ACTIVITY_W. What actually produces both numbers is taking the share over
// `window - DEFAULT_LEFT` when the code takes it over
// `window - DEFAULT_LEFT - ACTIVITY_W`, then charging the middle the full chrome
// anyway:
//
//   478 = 1440 - 240 - 48 - round((1440 - 240) * 0.56) - 2   [right 672, not 645]
//   408 = 1280 - 240 - 48 - round((1280 - 240) * 0.56) - 2   [right 582, not 556]
//
// i.e. the right pane came out 27px / 26px too wide and the middle lost exactly
// that, and nothing went red — because nothing was asserting on this shape. The
// expected values below are likewise derived from the constants, NOT read off a
// run:
//
//   beside = window - (DEFAULT_LEFT + ACTIVITY_W)
//   right  = round(beside * DEFAULT_RIGHT_SHARE)     [clamps bind at neither]
//   middle = beside - right
//
//   1440 -> beside 1152, right round(645.12) = 645, middle 507
//   1280 -> beside  992, right round(555.52) = 556, middle 436
//
// What this block does NOT prove: that App.tsx still passes the sum. These are
// assertions about layout.ts given the production shape; the call sites
// (App.tsx `rightW` initializer and `resizeRight`) have no test of their own —
// App.test.tsx asserts nothing about widths. Drop `+ ACTIVITY_W` from both call
// sites and the ENTIRE suite stays green (measured, tether#100 review). Said
// plainly so the next reader does not mistake this for end-to-end coverage;
// closing that half means folding ACTIVITY_W into the functions themselves so
// the omission stops being representable, which is its own change — tether#102.
//
// Out of scope on purpose: 505 and 434, the widths ForceGraph actually measures.
// Those are 507/436 less the two 1px `.col-resizer` dividers, and that 2px is a
// stylesheet literal (index.css `.col-resizer`), not something layout.ts knows
// or should know. Hard-coding it here would make this file's arithmetic depend
// on a number no function in it reads — a guard that cannot fail for the reason
// it names. To be clear about what that costs: NO test anywhere asserts 505 or
// 434. ForceGraph.test.tsx derives them in prose, which is a different thing.
describe('the production parameter shape (App.tsx passes leftWidth + ACTIVITY_W)', () => {
  /** All the chrome left of the middle column — what App.tsx actually passes. */
  const CHROME = DEFAULT_LEFT + ACTIVITY_W

  it('is the tree plus the activity bar, not the tree alone', () => {
    expect(CHROME).toBe(288)
    expect(CHROME).not.toBe(DEFAULT_LEFT)
  })

  // The pair that makes the omission visible: same viewport, same function, 27px
  // apart. 672 (asserted far above) is the variant without the activity bar.
  it('yields a different width than the leftWidth-only variant does', () => {
    expect(defaultRightWidth(1440, DEFAULT_LEFT)).toBe(672)
    expect(defaultRightWidth(1440, CHROME)).toBe(645)
  })

  it.each([
    [1440, 645, 507],
    [1280, 556, 436],
  ])(
    'gives the right pane %ipx -> %i and the middle %i on a first visit',
    (windowWidth, right, middle) => {
      // The whole chain App.tsx runs with nothing persisted.
      expect(loadRightWidth(null, windowWidth, CHROME)).toBe(right)
      // Same number unclamped: neither MIN/MAX_RIGHT nor the MIN_MID rule binds
      // at these viewports, which is why the share alone predicts the result.
      expect(defaultRightWidth(windowWidth, CHROME)).toBe(right)
      // What the middle column is left with, before divider chrome — the
      // quantity DEFAULT_RIGHT_SHARE's own note in layout.ts claims is 507/436.
      expect(mid(windowWidth, CHROME, right)).toBe(middle)
      // tether#71's property, restated on the shape production uses.
      expect(right).toBeGreaterThan(middle)
      expect(middle).toBeGreaterThanOrEqual(MIN_MID)
    },
  )

  // The drag path (App.tsx `resizeRight`), and the claim its comment makes: pass
  // leftW without ACTIVITY_W and the MIN_MID floor sits exactly 48px too loose.
  // Asserted because that comment is the only thing standing between the next
  // reader and re-introducing the bug.
  it('holds the MIN_MID floor exactly, where the leftWidth-only shape misses it', () => {
    const dragged = clampRightWidth(99999, 1000, CHROME)
    expect(dragged).toBe(392)
    expect(mid(1000, CHROME, dragged)).toBe(MIN_MID)

    // Same window, activity bar forgotten: 440 is permitted, and the middle it
    // really leaves is 272 — the floor missed by exactly the width of the bar.
    // (App.tsx's comment says 270 for the same case; that is 272 less the two
    // 1px dividers, a different quantity, not a disagreement.)
    const tooLoose = clampRightWidth(99999, 1000, DEFAULT_LEFT)
    expect(tooLoose).toBe(440)
    expect(mid(1000, CHROME, tooLoose)).toBeLessThan(MIN_MID)
    expect(tooLoose - dragged).toBe(ACTIVITY_W)
  })
})
