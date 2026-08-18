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
 * 1440 - 288 - 640 = 512 for the middle, where 288 is chromeLeftOfMiddle() at
 * the default tree — because a fallback that quietly reintroduces the bug it
 * exists beside is worse than no fallback.
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
 *   right > middle  <=>  DEFAULT_RIGHT_ish > (windowWidth - chrome) / 2
 *
 * ...where `chrome` is chromeLeftOfMiddle(treeWidth), 288 at the defaults.
 *
 * A scalar default satisfies that at exactly one viewport. 560 (what this
 * replaced) failed at 1440: it leaves the middle 592, the wider pane. That
 * failure was first written down here as "the middle got 640", which is
 * 1440 - 240 - 560 — the arithmetic from before tether#90 added the activity
 * bar, when the tree was all the chrome there was. Re-derived against the
 * chrome this module now charges it is 592, and 592 > 560, so the conclusion
 * survives the restatement; only the margin shrinks. Any scalar large enough
 * for 1920 pins the middle to MIN_MID on a 1280 laptop, and since MAX_RIGHT
 * caps at 1000, no scalar at all can hold past a 2288px window — the point
 * where 1000 stops exceeding half of `2288 - 288`. (That threshold reads
 * ~2240 in versions of this note before tether#102, which is the same
 * inequality solved against a 240px chrome.) A share holds everywhere the
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
 *   507px at a 1440 window, 436px at 1280 (windowWidth less
 *   chromeLeftOfMiddle(DEFAULT_LEFT), less this share), before divider
 *   chrome — so at two present columns the graph is at its cap either way.
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
 * Width of the activity bar, mirroring `.dt-activity` in index.css. Duplicated
 * because the rules below are arithmetic over the space the middle column is
 * left with, and that arithmetic cannot read a stylesheet. Change both.
 *
 * DELIBERATELY NOT EXPORTED — that is most of what tether#102 bought. It used
 * to be public, and both production call sites passed `<tree> + ACTIVITY_W` as
 * the third argument to the rules below. Dropping the addend at either call
 * site produced a plausible width and turned nothing red anywhere in the suite
 * (measured, tether#100 review; re-measured at the head of tether#102 for the
 * one-site case, which is the one that survives a typecheck). It is now added
 * by chromeLeftOfMiddle() on every path, and App.tsx can no longer name it:
 * writing `+ ACTIVITY_W` at a call site is a compile error rather than a
 * silent double-count.
 *
 * Unlike the policy constants above it is not a number to argue about; it is a
 * measurement of a DOM element, and the only number in this file that is wrong
 * the moment index.css disagrees with it — nothing checks that. A pure CSS
 * change to `.dt-activity`'s width turns no test red, here or anywhere else.
 * layout.test.ts does pin 48 through the public functions, but as its own
 * literal, deliberately not imported from here: an assertion phrased in terms
 * of this constant is immune to this constant's VALUE, which is the only thing
 * about it worth pinning. So the CSS pairing is still a convention — held up
 * by two comments and, now, one test that fails if this number moves alone.
 */
const ACTIVITY_W = 48

/**
 * All the chrome to the left of the middle column for a given workspace-tree
 * width — the quantity both rules below are really arithmetic on.
 *
 * They take a TREE width and add the bar here. Callers used to pass the sum,
 * which is what made "forgot the activity bar" a thing a caller could express
 * at all (tether#90 shipped that bug; tether#99 found two wrong widths written
 * down because of it; tether#100 documented the gap without closing it). One
 * function rather than the addend written twice, so the two rules cannot drift
 * apart from each other either.
 *
 * A negative tree width is a bogus measurement — App.tsx's `loadWidth` returns
 * `Number(localStorage.getItem(key))` with no validation, so a corrupt entry
 * reaches here — and it clamps to NO TREE, not to no chrome. The bar is
 * unconditional markup: the ≤768px block in index.css hides `.dt-left` and
 * both resizers and leaves `.dt-activity` standing. The other placement,
 * `Math.max(0, treeWidth + ACTIVITY_W)`, would let a garbage tree width delete
 * a 48px element that is definitely on screen — the same under-count of the
 * middle's chrome this function exists to make unexpressible, coming back in
 * through another door. layout.test.ts pins the difference.
 *
 * NaN is NOT handled: it propagates and both rules return NaN. Unchanged by
 * tether#102 (the old code was handed `NaN + ACTIVITY_W`, equally NaN) and
 * left alone rather than fixed in passing — different defect, different blast
 * radius, and no test or caller currently depends on either answer.
 *
 * What this still does not count, and deliberately — re-derived here rather
 * than carried over, because `.dt-activity` is `box-sizing: border-box` so its
 * 1px `border-right` is INSIDE the 48 and is not one of the missing pixels:
 *
 *   1px — what this function under-reports. Real chrome left of the middle is
 *         bar + tree + the one `.col-resizer` between them = 289 at the
 *         defaults; this returns 288. (The bar has no resizer beside it — it
 *         is a fixed 48.)
 *   2px — the slack in the MIN_MID guarantee, because App.tsx renders a second
 *         ColResizer on the middle's OTHER side and that is unaccounted too.
 *         Where the floor binds, the middle gets 318 rather than the promised
 *         320. (It can be far less where no width satisfies both panes — see
 *         clampRightWidth's over-constrained branch: window 900 with a 480px
 *         tree leaves room 52, so MIN_RIGHT wins and the middle gets
 *         900 - 528 - 260 = 112, or 110 after the dividers. That is the
 *         deliberate MIN_RIGHT-wins case, not this 2px. This example read 158
 *         before tether#102 — that was 160 - 2 with `leftWidth` 480 meaning
 *         the whole chrome rather than the tree, so it does not carry over.)
 *         The same 2px is why the 507 quoted above is 2 over what the middle
 *         really gets, 505.
 *
 * Folding the resizers in too was considered for tether#102 and rejected. The
 * reason is NOT "layout.ts must not hold a stylesheet literal" — ACTIVITY_W is
 * one, so that rule would have to have blocked tether#100 as well. It is:
 *
 *   1. Different defect. ACTIVITY_W was an addend a CALLER had to remember, so
 *      it had a wrong version that typechecked. The resizers are unaccounted
 *      uniformly, by this module, for every caller — there is no call site
 *      that can get them wrong. Making a bug unexpressible and making a floor
 *      exact are different jobs, and only the first is this item.
 *   2. Folding the bar is behaviour-preserving at every non-negative tree
 *      width: same inputs, same outputs, which is why it needs no owner
 *      decision. Folding the resizers moves every computed width by 1-2px and
 *      re-clamps every persisted one. That is a product change, and it wants
 *      its own item and its own decision rather than a ride on a refactor.
 */
function chromeLeftOfMiddle(treeWidth: number): number {
  return Math.max(0, treeWidth) + ACTIVITY_W
}

/**
 * defaultRightWidth is the right-pane width for a browser that has never
 * dragged the divider: a share of whatever sits beside the left chrome.
 *
 * `treeWidth` is the WORKSPACE TREE alone (App.tsx `leftW` / `.dt-left`). The
 * activity bar is added here, by chromeLeftOfMiddle — do not add it at the
 * call site; see that function.
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
export function defaultRightWidth(windowWidth: number, treeWidth: number): number {
  if (!Number.isFinite(windowWidth) || windowWidth <= 0) return DEFAULT_RIGHT
  const beside = windowWidth - chromeLeftOfMiddle(treeWidth)
  return Math.round(beside * DEFAULT_RIGHT_SHARE)
}

/**
 * clampRightWidth returns the right-pane width to actually use.
 *
 * Bounded by MIN_RIGHT/MAX_RIGHT, and additionally by what is left over after
 * the chrome to the middle's left — so the middle pane keeps at least MIN_MID.
 * That second bound depends on the *current* window, which is why this cannot
 * be a pair of constants: a width that is fine on a 2560px monitor crushes the
 * middle pane on a 1280px laptop, and the width is persisted across both.
 *
 * `treeWidth` is the WORKSPACE TREE alone; the activity bar is added here, by
 * chromeLeftOfMiddle. See that function.
 *
 * A non-finite or non-positive windowWidth (jsdom before layout, a hidden tab)
 * yields the constant bounds only — better to skip the window-dependent clamp
 * than to compute a nonsense width from a bogus measurement.
 *
 * The MIN_MID guarantee is conditional, and deliberately so: when the window
 * cannot fit chromeLeftOfMiddle(treeWidth) + MIN_RIGHT + MIN_MID at all, no
 * width satisfies both panes, and MIN_RIGHT wins. Shrinking the right pane below
 * MIN_RIGHT to protect the middle would trade one unusable pane for two.
 * layout.test.ts asserts both branches.
 */
export function clampRightWidth(desired: number, windowWidth: number, treeWidth: number): number {
  const bounded = Math.max(MIN_RIGHT, Math.min(MAX_RIGHT, Math.round(desired) || MIN_RIGHT))
  if (!Number.isFinite(windowWidth) || windowWidth <= 0) return bounded

  const room = windowWidth - chromeLeftOfMiddle(treeWidth) - MIN_MID
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
  treeWidth: number,
): number {
  const parsed = stored !== null ? Number(stored) : NaN
  const desired =
    Number.isFinite(parsed) && parsed > 0 ? parsed : defaultRightWidth(windowWidth, treeWidth)
  return clampRightWidth(desired, windowWidth, treeWidth)
}
