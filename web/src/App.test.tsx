import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import App from './App'
import { useStore } from './lib/store'
import { MIN_MID } from './lib/layout'

// The shell's leaf panes are stubbed. Not to make the test easier — ChatPane
// opens a WebTransport connection on mount and ShellPane spawns a PTY view — but
// because what is under test is App's ROUTING: which surface it puts in the
// middle column and which tabs it offers on the right. Each stub is a marker, so
// "the middle swapped" is an observation about App, not about Canvas or WorkPane.
vi.mock('./panes/chat', () => ({ default: () => <div data-testid="pane-chat" /> }))
vi.mock('./panes/shell', () => ({ default: () => <div data-testid="pane-shell" /> }))
vi.mock('./panes/skill', () => ({ default: () => <div data-testid="pane-skill" /> }))
vi.mock('./panes/workspace', () => ({ default: () => <div data-testid="pane-workspace" /> }))
vi.mock('./panes/canvas', () => ({ default: () => <div data-testid="pane-canvas" /> }))
// The Work stub echoes its `active` prop so the keep-alive test can assert that a
// pane kept alive behind another view is told it is not the visible one — that
// flag is what stops DetailDrawer's document-level Esc from firing off-screen.
vi.mock('./panes/work', () => ({
  default: ({ active }: { active?: boolean }) => (
    <div data-testid="pane-work" data-active={String(active)} />
  ),
}))
vi.mock('./lib/version', () => ({ useAppVersion: () => 'v-test' }))

afterEach(() => {
  cleanup()
  localStorage.clear()
  useStore.getState().select(null)
  // tether#129 — the theme lives on documentElement, which cleanup() does not
  // touch. A test that turns it dark would otherwise hand the next one a dark
  // document it never asked for.
  document.documentElement.removeAttribute('data-theme')
})

const activityBtn = (label: string) => screen.getByRole('button', { name: label })
const tabLabels = (c: HTMLElement) =>
  [...c.querySelectorAll('.dt-right-tab')].map((b) => b.textContent)
const mid = (c: HTMLElement) => c.querySelector('.dt-mid') as HTMLElement
// Which main view is actually SHOWN. Presence in the DOM is no longer the same
// question: a visited view stays mounted and is hidden with display:none, which
// is the whole point of the keep-alive.
const shownInMid = (c: HTMLElement) =>
  [...mid(c).querySelector('.dt-mid-stack')!.children]
    .filter((el) => (el as HTMLElement).style.display !== 'none')
    .map((el) => el.querySelector('[data-testid]')?.getAttribute('data-testid'))

describe('activity bar (tether#90)', () => {
  it('renders one icon-only entry per main view, with an accessible name', () => {
    const { container } = render(<App />)
    const btns = [...container.querySelectorAll('.dt-activity-btn')]
    expect(btns.map((b) => b.getAttribute('aria-label'))).toEqual(['Canvas', 'Work'])
    expect(btns.map((b) => b.getAttribute('title'))).toEqual(['Canvas', 'Work'])
    // icon-only: the tooltip carries the word, the button does not
    expect(btns.map((b) => b.textContent)).toEqual(['', ''])
  })

  // The wiring hop. Not "clicking sets state" and not "WorkPane renders" — that
  // the middle column's CONTENT changes as a result of the click.
  it('clicking an entry swaps what the middle column renders', () => {
    const { container } = render(<App />)
    expect(shownInMid(container)).toEqual(['pane-canvas'])
    fireEvent.click(activityBtn('Work'))
    expect(shownInMid(container)).toEqual(['pane-work'])
    fireEvent.click(activityBtn('Canvas'))
    expect(shownInMid(container)).toEqual(['pane-canvas'])
  })

  it('marks the selected entry and moves the mark with the selection', () => {
    const { container } = render(<App />)
    const on = () => container.querySelector('.dt-activity-btn.on')?.getAttribute('aria-label')
    expect(on()).toBe('Canvas')
    expect(activityBtn('Canvas').getAttribute('aria-current')).toBe('page')
    fireEvent.click(activityBtn('Work'))
    expect(on()).toBe('Work')
    expect(activityBtn('Canvas').getAttribute('aria-current')).toBeNull()
  })

  it('Work does not take Chat away with it', () => {
    const { container } = render(<App />)
    fireEvent.click(activityBtn('Work'))
    expect(container.querySelector('.dt-right')).not.toBeNull()
    expect(screen.getByTestId('pane-chat')).toBeTruthy()
  })

  // `.dt-grid.mv-work` is what the ≤768px rules key off to decide which single
  // pane a phone shows. jsdom applies no media queries, so this pins the hook
  // those rules depend on and nothing about the resulting layout. (`mv-canvas`
  // has no rule of its own today — it is published for symmetry so a future rule
  // does not have to invert the selector.)
  it('publishes the active view on .dt-grid for the responsive rules', () => {
    const { container } = render(<App />)
    expect(container.querySelector('.dt-grid')!.className).toContain('mv-canvas')
    fireEvent.click(activityBtn('Work'))
    expect(container.querySelector('.dt-grid')!.className).toContain('mv-work')
  })

  // BLOCKER from review: the middle column used to remount on every switch,
  // throwing away WorkGraphView's filters, search and fetched graph and paying
  // two fetches to get back. A marker pane cannot show state loss directly, so
  // this asserts the mechanism that prevents it — the node survives the switch
  // rather than being recreated — plus the `active` flag that has to go false
  // with it, because a live-but-hidden pane is exactly what makes DetailDrawer's
  // Esc guard load-bearing.
  it('keeps a visited view mounted (hidden) instead of tearing it down', () => {
    const { container } = render(<App />)
    fireEvent.click(activityBtn('Work'))
    const workNode = container.querySelector('[data-testid="pane-work"]')
    expect(workNode).not.toBeNull()

    fireEvent.click(activityBtn('Canvas'))
    const stillThere = container.querySelector('[data-testid="pane-work"]')
    expect(stillThere).toBe(workNode) // same DOM node ⇒ never unmounted
    expect((stillThere!.parentElement as HTMLElement).style.display).toBe('none')
    expect(stillThere!.getAttribute('data-active')).toBe('false')

    fireEvent.click(activityBtn('Work'))
    expect(container.querySelector('[data-testid="pane-work"]')).toBe(workNode)
    expect(workNode!.getAttribute('data-active')).toBe('true')
  })

  it('does not mount Work at all until it is first visited', () => {
    const { container } = render(<App />)
    expect(container.querySelector('[data-testid="pane-work"]')).toBeNull()
    fireEvent.click(activityBtn('Work'))
    expect(container.querySelector('[data-testid="pane-work"]')).not.toBeNull()
  })

  it('the middle breadcrumb names the view actually on screen', () => {
    const { container } = render(<App />)
    expect(container.querySelector('.dt-breadcrumb .crumb')?.textContent).toBe('workspace')
    fireEvent.click(activityBtn('Work'))
    expect(container.querySelector('.dt-breadcrumb .crumb')?.textContent).toBe('work')
  })

  it('remembers the view across a reload', () => {
    const { unmount } = render(<App />)
    fireEvent.click(activityBtn('Work'))
    expect(localStorage.getItem('tether_main_view')).toBe('work')
    unmount()
    const second = render(<App />)
    expect(shownInMid(second.container)).toEqual(['pane-work'])
  })

  it('mounts on the stored view, and on Canvas for a garbage one', () => {
    localStorage.setItem('tether_main_view', 'work')
    const first = render(<App />)
    expect(shownInMid(first.container)).toEqual(['pane-work'])
    cleanup()
    localStorage.setItem('tether_main_view', 'nonsense')
    const second = render(<App />)
    expect(shownInMid(second.container)).toEqual(['pane-canvas'])
  })
})

// BLOCKER from review: Canvas is the only reader of store.selectedFile, so once
// the middle column could show Work instead, clicking a file in the left tree
// set state that nothing rendered — a dead click on the primary desktop path.
// These drive the store the way WorkspaceTree does (select({ file })), not a
// click on the mocked tree, so what is under test is App's reaction to the
// selection rather than the tree's ability to produce one.
describe('picking a file brings the file view back (tether#90)', () => {
  const pick = (path: string) =>
    useStore.getState().select({ file: { wsId: 'w1', path } })

  it('switches the middle column back to Canvas', () => {
    const { container } = render(<App />)
    fireEvent.click(activityBtn('Work'))
    expect(shownInMid(container)).toEqual(['pane-work'])
    act(() => pick('README.md'))
    expect(shownInMid(container)).toEqual(['pane-canvas'])
    expect(container.querySelector('.dt-activity-btn.on')?.getAttribute('aria-label')).toBe('Canvas')
  })

  it('re-picking the file you are already on still comes back', () => {
    const { container } = render(<App />)
    act(() => pick('README.md'))
    fireEvent.click(activityBtn('Work'))
    expect(shownInMid(container)).toEqual(['pane-work'])
    act(() => pick('README.md')) // same path, fresh object from store.select
    expect(shownInMid(container)).toEqual(['pane-canvas'])
  })

  // Selecting a WI must NOT yank you out of Work: store.select only writes the
  // fields present in its argument, and the Work map passes { wiId } alone.
  it('selecting a wi does not switch away from Work', () => {
    const { container } = render(<App />)
    fireEvent.click(activityBtn('Work'))
    act(() => useStore.getState().select({ wiId: 'wi_x' }))
    expect(shownInMid(container)).toEqual(['pane-work'])
  })
})

describe('right pane is back to three tabs (tether#90 restores freeze rule 1)', () => {
  it('offers exactly Chat / Skills / Shell', () => {
    const { container } = render(<App />)
    expect(tabLabels(container)).toEqual(['Chat', 'Skills', 'Shell'])
  })

  it('has no Work tab, and no Work pane inside the right column', () => {
    const { container } = render(<App />)
    expect(tabLabels(container)).not.toContain('Work')
    expect(container.querySelector('.dt-right')!.querySelector('[data-testid="pane-work"]')).toBeNull()
  })
})

// The most likely real regression from this change: a browser that stored the
// name of a tab that no longer exists. loadRightTab is the whole migration and
// is unit-tested in test/app-tab.spec.ts; what only a mount can show is the
// consequence — that the restored value actually selects a button that is on
// screen, rather than leaving every tab unselected and the body blank.
describe('persisted legacy right-tab, at mount (tether#90)', () => {
  it('mounts on Chat from a stored "work", with exactly one tab selected', () => {
    localStorage.setItem('tether_right_tab', 'work')
    const { container } = render(<App />)
    expect([...container.querySelectorAll('.dt-right-tab.on')].map((b) => b.textContent)).toEqual(['Chat'])
    expect(screen.getByTestId('pane-chat')).toBeTruthy()
  })

  it('mounts with a selected tab for every value a browser could be holding', () => {
    for (const stored of ['work', 'chat', 'skill', 'shell', 'bogus']) {
      localStorage.setItem('tether_right_tab', stored)
      const { container } = render(<App />)
      expect(container.querySelectorAll('.dt-right-tab.on').length).toBe(1)
      cleanup()
    }
  })
})

// panes/canvas's "Pick a wi" home action dispatches this event with 'work'. That
// file is outside tether#90's scope, so App honours the name by routing it to the
// middle column instead of leaving it to select a tab that no longer exists.
describe('tether:select-tab is routed by surface name (tether#90)', () => {
  it("routes 'work' to the middle column", () => {
    const { container } = render(<App />)
    fireEvent(window, new CustomEvent('tether:select-tab', { detail: 'work' }))
    expect(shownInMid(container)).toEqual(['pane-work'])
  })

  it("still routes 'chat' and 'shell' to the right pane", () => {
    const { container } = render(<App />)
    fireEvent(window, new CustomEvent('tether:select-tab', { detail: 'shell' }))
    expect([...container.querySelectorAll('.dt-right-tab.on')].map((b) => b.textContent)).toEqual(['Shell'])
    fireEvent(window, new CustomEvent('tether:select-tab', { detail: 'chat' }))
    expect([...container.querySelectorAll('.dt-right-tab.on')].map((b) => b.textContent)).toEqual(['Chat'])
  })

  it('ignores a name that matches no surface, rather than blanking the pane', () => {
    const { container } = render(<App />)
    fireEvent(window, new CustomEvent('tether:select-tab', { detail: 'nonsense' }))
    expect([...container.querySelectorAll('.dt-right-tab.on')].map((b) => b.textContent)).toEqual(['Chat'])
    expect(shownInMid(container)).toEqual(['pane-canvas'])
  })
})

// tether#129 defect 1 — the MIN_MID guarantee held on ONE of the two dividers.
//
// `resizeRight` routed through lib/layout's clampRightWidth, which knows the
// window and the tree width and therefore knows what the middle column is left
// with. `resizeLeft`, eight lines above it, clamped to MIN_LEFT/MAX_LEFT alone:
// it read neither the window nor the right pane, so dragging the tree out to
// MAX_LEFT walked straight through the floor the sibling handler exists to hold.
//
// THE WIRING HOP, not the rule. layout.test.ts owns clampLeftWidth's arithmetic;
// what only a mount can show is that the divider the user actually drags is the
// one calling it — App.tsx could hold a correct rule and still clamp with the
// wrong one, or with none, and every unit test in layout.test.ts would stay
// green. (That is this repo's standing blind spot: see the note above
// layout.test.ts's last describe block for the same lesson learned about the
// activity-bar addend.)
//
// The numbers, derived rather than read off a run — window 1280, nothing
// persisted, so leftW = DEFAULT_LEFT = 240 and rightW = loadRightWidth(null,
// 1280, 240) = 556 (layout.test.ts pins that pair):
//
//   before: the tree reaches MAX_LEFT = 480 and the middle gets
//           1280 - (480 + 48) - 556 = 196, against a promised floor of 320.
//   after:  the tree stops at 1280 - 48 - 556 - 320 = 356 and the middle gets
//           exactly 320.
//
// 196 is measured through this test, not asserted from the constants: the
// pre-fix run of the assertion below reported `expected 480 to be 356`.
describe('the left divider holds the MIN_MID floor too (tether#129)', () => {
  const realWidth = window.innerWidth
  afterEach(() => { window.innerWidth = realWidth })

  const px = (el: Element | null) => Number((el as HTMLElement).style.width.replace('px', ''))
  const treeW  = (c: HTMLElement) => px(c.querySelector('.dt-left'))
  const rightW = (c: HTMLElement) => px(c.querySelector('.dt-right'))
  // What the middle column is left with. The activity bar is a fixed 48px of
  // chrome to its left (index.css `.dt-activity`); written as a literal for the
  // same reason layout.test.ts writes it as one — an expectation phrased in
  // terms of the constant is immune to the constant's value.
  const midW = (c: HTMLElement) => window.innerWidth - (treeW(c) + 48) - rightW(c)

  /** Drag a divider. index 0 is the one left of the middle column, 1 the one right of it. */
  const dragDivider = (c: HTMLElement, index: number, dx: number) => {
    fireEvent.mouseDown(c.querySelectorAll('.col-resizer')[index], { clientX: 0 })
    fireEvent.mouseMove(document, { clientX: dx })
    fireEvent.mouseUp(document)
  }

  it('stops the tree short of MAX_LEFT rather than crushing the middle', () => {
    window.innerWidth = 1280
    const { container } = render(<App />)
    expect(treeW(container)).toBe(240)
    expect(rightW(container)).toBe(556)

    dragDivider(container, 0, 1000) // far past MAX_LEFT (480)

    expect(treeW(container)).toBe(356)
    expect(midW(container)).toBe(MIN_MID)
  })

  // The property, over viewports rather than at one — a single-window assertion
  // is the shape of bug this whole module exists to stop (see loadRightWidth's
  // tether#71 table). Both directions are dragged: shrinking the tree cannot
  // violate the floor, and asserting it anyway is what keeps the new clamp from
  // being written as a bound on the wrong side.
  it.each([1280, 1366, 1440, 1600, 1920])('holds the floor at %ipx, dragged either way', (w) => {
    window.innerWidth = w
    const { container } = render(<App />)
    for (const dx of [1000, -1000, 400, -400]) {
      dragDivider(container, 0, dx)
      expect(midW(container)).toBeGreaterThanOrEqual(MIN_MID)
    }
    cleanup()
  })

  // The guarantee is one-directional on purpose, and this is the assertion that
  // says so. The left divider sits between the tree and the middle, so a drag on
  // it may shrink the middle down to its floor and then stop — it must NOT reach
  // across the middle and take width off the right pane instead. That would be a
  // divider moving a column it is not adjacent to, and it is also the shape
  // clampRightWidth already rejected in the mirror case: it caps the right pane
  // rather than shrinking the tree.
  it('does not pay for the floor out of the right pane', () => {
    window.innerWidth = 1280
    const { container } = render(<App />)
    const before = rightW(container)
    dragDivider(container, 0, 1000)
    expect(rightW(container)).toBe(before)
  })
})

// tether#129 defect 4 — Settings' dark-mode flag was a MOUNT SNAPSHOT.
//
// `useState(document.documentElement.getAttribute('data-theme') === 'dark')`
// samples the theme once, on open, and then nothing re-reads it. The other writer
// is main.tsx's document-level ⌘⇧D / Ctrl+Shift+D handler, which sets that
// attribute directly and knows nothing about this panel. So pressing the shortcut
// with Settings open really did flip the theme while the copy went stale: the
// appearance row's sub-label read the opposite of the screen, and `toggleTheme`
// computed `!isDark` from the stale value, so the switch could ask for the theme
// already in effect and appear to do nothing.
//
// Driven through App, because that is where Settings actually mounts, and asserted
// on what a user can see — the nav row's sub-label and the switch's `on` class —
// rather than on the hook.
//
// The other writer is simulated as a bare setAttribute, which is exactly what
// main.tsx does, and deliberately not as a tether-specific event: main.tsx is the
// entry module and calls createRoot at import time, so it cannot be imported
// here, and the claim under test is that Settings follows the ATTRIBUTE whoever
// wrote it. A test that went through an event would only prove Settings follows
// that event.
//
// Every mutation is awaited inside `act` because MutationObserver delivers its
// records on a MICROTASK, not synchronously. That is a real one-microtask delay
// and not a test artefact — it is also why it is invisible in a browser, where the
// checkpoint runs before the next paint.
describe('Settings tracks the theme it does not own (tether#129)', () => {
  const openSettings = () => {
    // Settings fetches providers and skills on mount; both are caught, but stub
    // them so this is not making network calls to a relative URL.
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [] })))
    const { container } = render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }))
    return container
  }

  const navBtn = (c: HTMLElement, name: string) =>
    [...c.querySelectorAll('.settings-nav-btn')]
      .find((b) => b.querySelector('.settings-nav-name')?.textContent === name)!

  const navSub = (c: HTMLElement, name: string) =>
    navBtn(c, name).querySelector('.settings-nav-sub')?.textContent

  /** Open Settings on the appearance sub-page, where the switch lives. */
  const openAppearance = () => {
    const c = openSettings()
    fireEvent.click(navBtn(c, 'appearance'))
    return c
  }

  const toggle = (c: HTMLElement) => c.querySelector('.set-toggle')

  /** What main.tsx's ⌘⇧D handler does to the document, and nothing else. */
  const flipThemeElsewhere = (to: 'dark' | 'light') => act(async () => {
    if (to === 'dark') document.documentElement.setAttribute('data-theme', 'dark')
    else document.documentElement.removeAttribute('data-theme')
  })

  const clickSwitch = (c: HTMLElement) => act(async () => { fireEvent.click(toggle(c)!) })

  afterEach(() => { vi.unstubAllGlobals() })

  it('re-reads the theme when something else changes it', async () => {
    const c = openSettings()
    expect(navSub(c, 'appearance')).toBe('light')

    await flipThemeElsewhere('dark')
    expect(navSub(c, 'appearance')).toBe('dark')

    await flipThemeElsewhere('light')
    expect(navSub(c, 'appearance')).toBe('light')
  })

  it('moves the switch with it, not just the label', async () => {
    const c = openAppearance()
    expect(toggle(c)!.className).not.toContain('on')

    await flipThemeElsewhere('dark')
    expect(toggle(c)!.className).toContain('on')
  })

  // The half a user actually hits. Once the theme had moved underneath it,
  // Settings' switch computed its next value from the stale flag: the document
  // had gone dark, the flag still said light, so the switch asked for dark and
  // landed on the theme it was meant to leave.
  it('its own switch turns the theme OFF after something else turned it on', async () => {
    const c = openAppearance()

    await flipThemeElsewhere('dark')
    await clickSwitch(c)

    expect(document.documentElement.getAttribute('data-theme')).toBeNull()
    expect(localStorage.getItem('tether_theme')).toBe('light')
    expect(navSub(c, 'appearance')).toBe('light')
  })

  // Settings still WRITES the theme; only the reading changed. Without this a fix
  // that made the flag read-only would satisfy everything above.
  it('still writes the theme itself when nothing else has touched it', async () => {
    const c = openAppearance()

    await clickSwitch(c)
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
    expect(localStorage.getItem('tether_theme')).toBe('dark')
    expect(navSub(c, 'appearance')).toBe('dark')
    expect(toggle(c)!.className).toContain('on')

    await clickSwitch(c)
    expect(document.documentElement.getAttribute('data-theme')).toBeNull()
    expect(localStorage.getItem('tether_theme')).toBe('light')
    expect(toggle(c)!.className).not.toContain('on')
  })

  // Opening onto an already-dark document has to read dark. This is the one case
  // the mount snapshot got right, so it guards the fix rather than gating the bug.
  it('opens on the theme already in effect', () => {
    document.documentElement.setAttribute('data-theme', 'dark')
    const c = openSettings()
    expect(navSub(c, 'appearance')).toBe('dark')
  })

  // The subscription must not outlive the panel. Settings unmounts on close, and a
  // MutationObserver held past unmount would keep calling into a dead tree — the
  // failure mode is a React warning and a leak, neither of which shows up as a
  // wrong label. Asserted as "closing, then flipping the theme, is quiet".
  it('stops observing when the panel closes', async () => {
    const c = openSettings()
    const warn = vi.spyOn(console, 'error').mockImplementation(() => {})
    fireEvent.click(c.querySelector('.settings-header .icon-btn')!)
    expect(c.querySelector('.settings-panel')).toBeNull()

    await flipThemeElsewhere('dark')
    expect(warn).not.toHaveBeenCalled()
    warn.mockRestore()
  })
})
