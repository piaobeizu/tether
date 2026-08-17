// Column-width rules for the three-pane shell (tether#69).
//
// Extracted from App.tsx because this is the only part of the layout with an
// invariant worth guarding: whatever the user drags to, and whatever a previous
// session persisted, the middle pane must stay usable.

/** Right pane — the narrowest useful width. Its tabs are App.tsx `RIGHT_TABS`
 *  ('chat' | 'skill' | 'shell'; display names in `RIGHT_TAB_LABEL`). Work is not
 *  among them: it moved to the middle column in tether#90. */
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
 * Middle pane — App.tsx `MainView`, which is the canvas (itself the file view
 * once a file is selected) or the Work graph. Below this it stops being readable,
 * and since the right width is persisted, a bad value would come back on reload.
 */
export const MIN_MID = 320

/**
 * Fallback right-pane width for a browser that has never been resized AND whose
 * window cannot be measured.
 *
 * Prefer defaultRightWidth(), which knows the viewport. This constant only
 * covers the case that function cannot answer (jsdom before layout, a hidden
 * tab). It is still sized so the right pane wins on a nominal 1440px screen —
 * 1440 - 240 - 640 = 560 for the middle — because a fallback that quietly
 * reintroduces the bug it exists beside is worse than no fallback.
 */
export const DEFAULT_RIGHT = 640

/**
 * Share of the space beside the left pane that the right pane takes by default.
 *
 * Chat is the primary interaction and it lives in the right pane — it is that
 * pane's default tab (App.tsx `loadRightTab` falls back to 'chat') — so the
 * right pane is the primary column, and "primary" is a RATIO, not a number of
 * pixels. That distinction is the whole reason this is a share:
 *
 *   right > middle  <=>  DEFAULT_RIGHT_ish > (windowWidth - leftWidth) / 2
 *
 * A scalar default satisfies that at exactly one viewport. 560 (what this
 * replaced) failed at 1440 — the middle got 640. Any scalar large enough for
 * 1920 pins the middle to MIN_MID on a 1280 laptop, and since MAX_RIGHT caps at
 * 1000, no scalar at all can hold past ~2240. A share holds everywhere the
 * clamps leave room, and hands the decision back to the clamps where they do
 * not. Anything above 0.5 makes the right pane the widest; 0.56 does it with a
 * visible margin without squeezing the canvas.
 *
 * The premise above used to read "the right pane is where the work happens —
 * Work, Chat, Skills, Shell". tether#90 ended that: Work is a MIDDLE-column view
 * (App.tsx `MainView`, chosen from the activity bar), and the right pane is three
 * tabs (`RIGHT_TABS = ['chat', 'skill', 'shell']`). Only the premise changed —
 * 0.56 did not, and why it did not is worth writing down, because "Work moved to
 * the middle" reads like a reason to shrink the right pane. It is not:
 *
 *   panes/work/ForceGraph.tsx's computePositions sizes cards on `presentCols` —
 *   the status columns that actually HOLD nodes — not on all five statuses. With
 *   two present columns its arithmetic leaves the compact card form at exactly
 *   276px of graph width and reaches the 132px card cap at exactly 348px — the
 *   thresholds are `110 * nCols + 56` and `146 * nCols + 56`, and
 *   ForceGraph.test.tsx pins the first of those at three, four and five columns.
 *   The middle keeps the other 0.44 of the space beside the left pane —
 *   507px at a 1440 window, 436px at 1280 (windowWidth - DEFAULT_LEFT -
 *   ACTIVITY_W, less this share), before divider chrome — so at two present
 *   columns the graph is at its cap either way.
 *
 * Whether some larger number of PRESENT columns wants a wider middle is a fair
 * question, but it is a measurement against presentCols — not against the
 * five-status enum, which is what makes the middle look starved on paper when it
 * is not. Re-measure before touching this number, and do it in its own change.
 */
export const DEFAULT_RIGHT_SHARE = 0.56

/** Default left-pane (workspace tree) width. */
export const DEFAULT_LEFT = 240

/**
 * defaultRightWidth is the right-pane width for a browser that has never
 * dragged the divider: a share of whatever sits beside the left pane.
 *
 * UNCLAMPED on purpose — callers pass the result through clampRightWidth, which
 * owns MIN_RIGHT/MAX_RIGHT and the MIN_MID guarantee. Keeping the two apart
 * means the share never has to re-derive rules that already exist, and the
 * ceiling stays stated in one place: above roughly (MAX_RIGHT / share) of
 * available width the cap binds and the middle pane becomes the wider one
 * again, which is deliberate — a 1300px chat column is not a feature.
 *
 * A non-finite or non-positive windowWidth falls back to the constant, matching
 * clampRightWidth's rule for the same bogus measurement: better a fixed width
 * than one computed from a number known to be wrong.
 */
export function defaultRightWidth(windowWidth: number, leftWidth: number): number {
  if (!Number.isFinite(windowWidth) || windowWidth <= 0) return DEFAULT_RIGHT
  const beside = windowWidth - Math.max(0, leftWidth)
  return Math.round(beside * DEFAULT_RIGHT_SHARE)
}

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
 *
 * With nothing persisted the default comes from defaultRightWidth, so a first
 * visit gets a right pane sized to the actual viewport rather than to a number
 * that was only ever right on one monitor (tether#71). Note the default is fed
 * THROUGH clampRightWidth rather than around it — the share is a preference,
 * the clamp is the guarantee, and the guarantee wins.
 */
export function loadRightWidth(
  stored: string | null,
  windowWidth: number,
  leftWidth: number,
): number {
  const parsed = stored !== null ? Number(stored) : NaN
  const desired =
    Number.isFinite(parsed) && parsed > 0 ? parsed : defaultRightWidth(windowWidth, leftWidth)
  return clampRightWidth(desired, windowWidth, leftWidth)
}
