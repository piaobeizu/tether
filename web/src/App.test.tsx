import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import App from './App'
import { useStore } from './lib/store'

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
