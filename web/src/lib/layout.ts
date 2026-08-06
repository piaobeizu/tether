// Column-width rules for the three-pane shell (tether#69).
//
// Extracted from App.tsx because this is the only part of the layout with an
// invariant worth guarding: whatever the user drags to, and whatever a previous
// session persisted, the middle pane must stay usable.

/** Right pane (Chat / Work / Skills / Shell) — the narrowest useful width. */
export const MIN_RIGHT = 260

/**
 * Right pane maximum.
 *
 * Was 600, which made the middle pane structurally dominant: at a 1440px
 * viewport, left 240 + right 600 left the middle at 600 no matter what the user
 * dragged, so Chat — the primary interaction — could never be the widest pane.
 * The cap exists to stop the middle from vanishing, and MIN_MID now does that
 * job directly, so this can be generous.
 */
export const MAX_RIGHT = 1000

/**
 * Middle pane (canvas / file view) — below this it stops being readable, and
 * since the right width is persisted, a bad value would come back on reload.
 */
export const MIN_MID = 320

/** Default right-pane width for a browser that has never been resized. */
export const DEFAULT_RIGHT = 560

/** Default left-pane (workspace tree) width. */
export const DEFAULT_LEFT = 240

/**
 * clampRightWidth returns the right-pane width to actually use.
 *
 * Bounded by MIN_RIGHT/MAX_RIGHT, and additionally by what is left over after
 * the left pane — so the middle pane keeps at least MIN_MID. That second bound
 * depends on the *current* window, which is why this cannot be a pair of
 * constants: a width that is fine on a 2560px monitor crushes the middle pane
 * on a 1280px laptop, and the width is persisted across both.
 *
 * A non-finite or non-positive windowWidth (jsdom before layout, a hidden tab)
 * yields the constant bounds only — better to skip the window-dependent clamp
 * than to compute a nonsense width from a bogus measurement.
 *
 * The MIN_MID guarantee is conditional, and deliberately so: when the window
 * cannot fit leftWidth + MIN_RIGHT + MIN_MID at all, no width satisfies both
 * panes, and MIN_RIGHT wins. Shrinking the right pane below MIN_RIGHT to protect
 * the middle would trade one unusable pane for two. layout.test.ts asserts both
 * branches.
 */
export function clampRightWidth(desired: number, windowWidth: number, leftWidth: number): number {
  const bounded = Math.max(MIN_RIGHT, Math.min(MAX_RIGHT, Math.round(desired) || MIN_RIGHT))
  if (!Number.isFinite(windowWidth) || windowWidth <= 0) return bounded

  const room = windowWidth - Math.max(0, leftWidth) - MIN_MID
  // When the window is so narrow that even MIN_RIGHT leaves less than MIN_MID,
  // MIN_RIGHT wins: the right pane is where the user is working, and shrinking
  // it below usable would trade one broken pane for two.
  if (room < MIN_RIGHT) return MIN_RIGHT
  return Math.min(bounded, room)
}

/**
 * loadRightWidth reads the persisted right-pane width and clamps it for the
 * window it is being restored into.
 *
 * Clamping on read is not redundant with clamping on write: the value was
 * written against whatever window was open at the time, so a width stored on a
 * wide monitor would otherwise reproduce a crushed middle pane every time the
 * app loads on a narrower one — the classic gap in "persist + clamp" code,
 * where only the write path is guarded.
 */
export function loadRightWidth(
  stored: string | null,
  windowWidth: number,
  leftWidth: number,
): number {
  const parsed = stored !== null ? Number(stored) : NaN
  const desired = Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_RIGHT
  return clampRightWidth(desired, windowWidth, leftWidth)
}
