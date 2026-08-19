import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { AnswerBody, AnswerMeta, ThinkingBlock, ToolCallList, fmtThinkMs, fmtTokens, summarizeToolInput, summarizeToolResult, truncateResult, shouldSendOnEnter, growHeight, parseAtQuery, fuzzyRankFiles, shouldDeferFirstConnect, shouldReconnectAfterClose, shouldRefundAttemptBudget, transcriptTextLength, FATAL_CODE_MESSAGES, heldActivityLine, trailingArrivals, HELD_ACTIVITY_WORKING, HELD_ACTIVITY_IDLE, HELD_ACTIVITY_UNKNOWN, HELD_ACTIVITY_GONE } from './index'
import { SESSION_ACTIVITY_HELD, SESSION_ACTIVITY_IDLE, SESSION_ACTIVITY_WORKING } from '../../lib/sessionActivity'
import { PermissionQueue, postDecide } from '../../fenced-blocks/PermissionBlock'
import {
  ErrCodeUnknownWorkspace, ErrCodeNoWorkspaceRegistry, ErrCodeUnknownProvider, ErrCodeSessionOwned,
  ErrCodeSessionHeldByBackgroundAgent,
  ErrCodeSpawnFailed, ErrCodeConnectionClosed, ErrCodeSessionUnconfirmed, ErrCodeAgent,
  ErrCodePromptUndelivered,
} from '../../lib/wire.gen'
import type { ToolCall, PermissionRequest } from '../../lib/store'

// tether#34 — ThinkingBlock is exported and prop-controlled so it tests directly,
// without mounting ChatPane (which opens a WebTransport connection on mount).

// vitest globals are off (matches Canvas.test.tsx), so register cleanup explicitly.
afterEach(() => cleanup())

describe('fmtThinkMs (tether#34)', () => {
  it('formats whole seconds, sub-10s decimals, and minutes', () => {
    expect(fmtThinkMs(8000)).toBe('8s')
    expect(fmtThinkMs(1200)).toBe('1.2s')
    expect(fmtThinkMs(500)).toBe('0.5s')
    expect(fmtThinkMs(12000)).toBe('12s')
    expect(fmtThinkMs(90000)).toBe('1m 30s')
  })
  it('handles rounding boundaries (review MINOR): no "10.0s", no "1m 60s"', () => {
    expect(fmtThinkMs(9999)).toBe('10s')
    expect(fmtThinkMs(119500)).toBe('2m 0s')
  })
  it('returns empty string for undefined/negative durations', () => {
    expect(fmtThinkMs(undefined)).toBe('')
    expect(fmtThinkMs(-5)).toBe('')
  })
})

describe('ThinkingBlock (tether#34)', () => {
  it('renders live streaming thinking while live', () => {
    render(<ThinkingBlock thinking="pondering the plan" live expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('thinking…')).toBeTruthy()
    expect(screen.getByText('pondering the plan')).toBeTruthy()
  })

  it('collapses to a "thought Xs" summary once no longer live, hiding the text', () => {
    render(<ThinkingBlock thinking="secret reasoning" thinkingMs={8000} live={false} expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('thought 8s')).toBeTruthy()
    expect(screen.queryByText('secret reasoning')).toBeNull()
  })

  it('a collapsed thinking-only turn (no duration) still shows a bare "thought" summary', () => {
    render(<ThinkingBlock thinking="only thought" live={false} expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('thought')).toBeTruthy()
    expect(screen.queryByText('only thought')).toBeNull()
  })

  it('shows the thinking text when expanded', () => {
    render(<ThinkingBlock thinking="secret reasoning" thinkingMs={8000} live={false} expanded onToggle={() => {}} />)
    expect(screen.getByText('secret reasoning')).toBeTruthy()
  })

  it('clicking the collapsed summary calls onToggle', () => {
    const onToggle = vi.fn()
    render(<ThinkingBlock thinking="x" thinkingMs={8000} live={false} expanded={false} onToggle={onToggle} />)
    fireEvent.click(screen.getByText('thought 8s'))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  // tether#35 — thinking text renders as markdown in both live and expanded states.
  it('renders thinking markdown while live', () => {
    const { container } = render(<ThinkingBlock thinking={'weigh **A** vs B'} live expanded={false} onToggle={() => {}} />)
    expect(container.querySelector('.msg-thinking-text strong')?.textContent).toBe('A')
  })

  it('renders thinking markdown when expanded', () => {
    const { container } = render(<ThinkingBlock thinking={'- step one'} thinkingMs={8000} live={false} expanded onToggle={() => {}} />)
    expect(container.querySelector('.msg-thinking-text li')?.textContent).toBe('step one')
  })
})

describe('AnswerBody (tether#35)', () => {
  it('renders markdown bold and lists instead of raw markers', () => {
    const { container } = render(<AnswerBody text={'see **bold** and\n\n- one\n- two'} streaming={false} />)
    expect(container.querySelector('strong')?.textContent).toBe('bold')
    expect(container.querySelectorAll('li').length).toBe(2)
    expect(screen.queryByText('**bold**')).toBeNull() // raw markers must not appear
  })

  it('adds the streaming class only while streaming (drives the CSS cursor)', () => {
    const { container, rerender } = render(<AnswerBody text="hi" streaming />)
    expect(container.querySelector('.msg-ai-body.streaming')).toBeTruthy()
    rerender(<AnswerBody text="hi" streaming={false} />)
    expect(container.querySelector('.msg-ai-body.streaming')).toBeNull()
    expect(container.querySelector('.msg-ai-body')).toBeTruthy()
  })
})

describe('AnswerMeta (tether#36)', () => {
  const ts = 1784290000000

  it('shows the answer-duration badge when answerMs is set', () => {
    const { container } = render(<AnswerMeta ts={ts} answerMs={2100} />)
    const dur = container.querySelector('.msg-ai-dur')
    expect(dur).toBeTruthy()
    expect(dur?.textContent).toContain('2.1s')
    expect(container.querySelector('.msg-ai-name')?.textContent).toBe('tether')
  })

  it('omits the badge when answerMs is undefined (streaming / no answer)', () => {
    const { container } = render(<AnswerMeta ts={ts} answerMs={undefined} />)
    expect(container.querySelector('.msg-ai-dur')).toBeNull()
    expect(container.querySelector('.msg-ai-name')).toBeTruthy()
    expect(container.querySelector('.msg-ai-time')).toBeTruthy()
  })

  // tether#48 — token-usage badge.
  it('shows the token-usage badge when usage is set', () => {
    const { container } = render(<AnswerMeta ts={ts} answerMs={2100} usage={{ input: 1234, output: 856 }} />)
    const u = container.querySelector('.msg-ai-usage')
    expect(u).toBeTruthy()
    expect(u?.textContent).toContain('1.2k↑')
    expect(u?.textContent).toContain('856↓')
  })

  it('omits the token-usage badge when usage is undefined (live-only, e.g. after reload)', () => {
    const { container } = render(<AnswerMeta ts={ts} answerMs={2100} usage={undefined} />)
    expect(container.querySelector('.msg-ai-usage')).toBeNull()
    expect(container.querySelector('.msg-ai-dur')).toBeTruthy() // duration still shows
  })
})

// tether#48 — fmtTokens compacts a token count for the usage badge.
describe('fmtTokens (tether#48)', () => {
  it('renders sub-1k counts verbatim', () => {
    expect(fmtTokens(0)).toBe('0')
    expect(fmtTokens(856)).toBe('856')
    expect(fmtTokens(999)).toBe('999')
  })
  it('renders thousands with a "k" suffix (one decimal under ~10k, whole above)', () => {
    expect(fmtTokens(1000)).toBe('1k')     // lower k boundary
    expect(fmtTokens(1234)).toBe('1.2k')
    expect(fmtTokens(9949)).toBe('9.9k')   // just under the 9.95 threshold: decimal kept
    expect(fmtTokens(9950)).toBe('10k')    // >= 9.95k drops the decimal
    expect(fmtTokens(45678)).toBe('46k')
    expect(fmtTokens(45000)).toBe('45k')
  })
  it('renders millions with an "M" suffix', () => {
    expect(fmtTokens(1_400_000)).toBe('1.4M')
    expect(fmtTokens(2_000_000)).toBe('2M')
  })
  it('rolls the k→M seam cleanly instead of rendering "1000k" (tether#48 review M1/M2)', () => {
    // round(n/1000) would hit 1000 here; must render as M, not "1000k".
    expect(fmtTokens(999_999)).toBe('1.0M')
    expect(fmtTokens(1_000_000)).toBe('1M')   // whole M drops the decimal
    expect(fmtTokens(1_000_001)).toBe('1.0M') // just over: decimal kept, never "1000k"
  })
  it('returns empty string for undefined/negative', () => {
    expect(fmtTokens(undefined)).toBe('')
    expect(fmtTokens(-1)).toBe('')
  })
})

// tether#37 — tool-call visibility. summarizeToolInput + ToolCallList are exported
// pure functions/components so they test without mounting ChatPane (WebTransport).
describe('summarizeToolInput (tether#37)', () => {
  it('extracts the salient arg per known tool', () => {
    expect(summarizeToolInput('Read', { file_path: 'a/b.ts' })).toBe('a/b.ts')
    expect(summarizeToolInput('Bash', { command: 'go test ./...' })).toBe('go test ./...')
    expect(summarizeToolInput('Grep', { pattern: 'TODO' })).toBe('TODO')
    expect(summarizeToolInput('Edit', { file_path: 'x.go', old_string: '...' })).toBe('x.go')
  })
  it('returns empty for unknown tools, non-object input, or a missing/non-string field', () => {
    expect(summarizeToolInput('MysteryTool', { file_path: 'a.ts' })).toBe('')
    expect(summarizeToolInput('Read', null)).toBe('')
    expect(summarizeToolInput('Read', 'not-an-object')).toBe('')
    expect(summarizeToolInput('Read', {})).toBe('')
    expect(summarizeToolInput('Bash', { command: 123 })).toBe('')
  })
  it('collapses whitespace and truncates long values to 60 chars + …', () => {
    const out = summarizeToolInput('Bash', { command: 'x'.repeat(80) })
    expect(out.endsWith('…')).toBe(true)
    expect(out.length).toBe(61) // 60 chars + the ellipsis
    expect(summarizeToolInput('Bash', { command: 'a   b\n c' })).toBe('a b c')
  })
})

describe('ToolCallList (tether#37)', () => {
  const mk = (n: number): ToolCall[] =>
    Array.from({ length: n }, (_, i) => ({ id: `t${i}`, name: 'Read', input: { file_path: `f${i}.ts` } }))

  it('renders one row per tool with name + arg summary when few (<= threshold)', () => {
    const { container } = render(<ToolCallList tools={[
      { id: 't1', name: 'Read', input: { file_path: 'main.go' } },
      { id: 't2', name: 'Bash', input: { command: 'go build' } },
    ]} />)
    expect(container.querySelectorAll('.msg-tool-row').length).toBe(2)
    expect(screen.getByText('Read')).toBeTruthy()
    expect(screen.getByText('main.go')).toBeTruthy()
    expect(screen.getByText('go build')).toBeTruthy()
    expect(container.querySelector('.msg-tool-fold')).toBeNull() // no fold below threshold
  })

  it('renders nothing for an empty tool list', () => {
    const { container } = render(<ToolCallList tools={[]} />)
    expect(container.querySelector('.msg-tools')).toBeNull()
  })

  it('folds beyond the threshold into a "used N tools" summary, hiding rows until expanded', () => {
    const { container } = render(<ToolCallList tools={mk(8)} />)
    expect(container.querySelectorAll('.msg-tool-row').length).toBe(0) // collapsed by default
    const fold = container.querySelector('.msg-tool-fold')!
    expect(fold.textContent).toContain('used 8 tools')
    fireEvent.click(fold)
    expect(container.querySelectorAll('.msg-tool-row').length).toBe(8) // expanded
  })

  it('shows the tool name for an unknown tool (default icon, no arg)', () => {
    const { container } = render(<ToolCallList tools={[{ id: 'x', name: 'MysteryTool', input: {} }]} />)
    expect(screen.getByText('MysteryTool')).toBeTruthy()
    expect(container.querySelector('.msg-tool-arg')).toBeNull()
  })
})

// tether#38 — tool-result inlining: row-tail preview + click-to-expand block.
describe('summarizeToolResult (tether#38)', () => {
  it('summarizes per tool kind', () => {
    expect(summarizeToolResult('Read', { content: 'a\nb\nc', isError: false })).toBe('3 lines')
    expect(summarizeToolResult('Grep', { content: 'hit1\nhit2', isError: false })).toBe('2 matches')
    expect(summarizeToolResult('Grep', { content: 'only', isError: false })).toBe('1 match')
    expect(summarizeToolResult('Bash', { content: '  go version go1.25  \nextra', isError: false })).toBe('go version go1.25')
  })
  it('marks errors and empty content', () => {
    expect(summarizeToolResult('Bash', { content: 'boom', isError: true })).toBe('error')
    expect(summarizeToolResult('Read', { content: '', isError: false })).toBe('')
  })
})

describe('truncateResult (tether#38)', () => {
  it('leaves small results unchanged', () => {
    expect(truncateResult('one\ntwo')).toBe('one\ntwo')
  })
  it('clamps by line count with a marker', () => {
    const out = truncateResult(Array.from({ length: 30 }, (_, i) => `L${i}`).join('\n'))
    expect(out.split('\n').length).toBe(21) // 20 kept + the marker line
    expect(out.endsWith('…(truncated)')).toBe(true)
  })
  it('clamps by char count with a marker', () => {
    const out = truncateResult('x'.repeat(3000))
    expect(out.length).toBeLessThan(3000)
    expect(out.endsWith('…(truncated)')).toBe(true)
  })
})

describe('ToolCallList results (tether#38)', () => {
  const withResult = (isError = false): ToolCall[] => [
    { id: 't1', name: 'Read', input: { file_path: 'a.ts' }, result: { content: 'l1\nl2\nl3', isError } },
  ]
  it('shows a result preview and expands the full block on click', () => {
    const { container } = render(<ToolCallList tools={withResult()} />)
    expect(screen.getByText('3 lines')).toBeTruthy()                // tail preview
    expect(container.querySelector('.msg-tool-result')).toBeNull()  // collapsed by default
    fireEvent.click(container.querySelector('.msg-tool-row.clickable')!)
    expect(container.querySelector('.msg-tool-result')?.textContent).toContain('l1')
  })
  it('marks an error result with the err class on preview and block', () => {
    const { container } = render(<ToolCallList tools={withResult(true)} />)
    expect(container.querySelector('.msg-tool-preview.err')?.textContent).toBe('error')
    fireEvent.click(container.querySelector('.msg-tool-row.clickable')!)
    expect(container.querySelector('.msg-tool-result.err')).toBeTruthy()
  })
  it('a tool without a result is not clickable and shows no preview', () => {
    const { container } = render(<ToolCallList tools={[{ id: 'x', name: 'Read', input: { file_path: 'a.ts' } }]} />)
    expect(container.querySelector('.msg-tool-row.clickable')).toBeNull()
    expect(container.querySelector('.msg-tool-preview')).toBeNull()
  })

  it('a present-but-empty result is not clickable (review MINOR: no dead click)', () => {
    const { container } = render(<ToolCallList tools={[{ id: 'e', name: 'Bash', input: { command: 'true' }, result: { content: '', isError: false } }]} />)
    expect(container.querySelector('.msg-tool-row.clickable')).toBeNull()
    expect(container.querySelector('.msg-tool-caret')).toBeNull()
  })
})

// tether#97 — a FAILED result with no message.
//
// cc can write `is_error` with content that flattens to nothing (an empty array, or
// only image / tool_reference sub-blocks) and the daemon serves it on purpose:
// dropping it would make a failed call read as a successful one
// (session.ccMessage.errorResults). The row therefore has to keep saying "error"
// while no longer offering to expand into a blank block — `hasResult` asks about
// content, the preview asks about the flag.
//
// Both directions are asserted. Only pinning the empty case would leave the
// regression that produced this wi unguarded: reinstating `|| isError` in hasResult
// makes the empty row clickable again, and REMOVING the preview's isError branch
// silently turns a failure into a blank row — the more dangerous of the two, and
// invisible to a one-sided test.
//
// Measured on a real store the day this landed: 41 failures served across 39
// transcript windows, 0 of them empty. So this shape is one cc CAN write, not one it
// currently does, and these fixtures are the only place it exists.
describe('ToolCallList and a failure with no message (tether#97)', () => {
  const failure = (content: string): ToolCall[] => [
    { id: 'f1', name: 'Bash', input: { command: 'false' }, result: { content, isError: true } },
  ]

  // The positive CONTROL, not a new guard: it passes on the old code too. It is
  // here because a one-sided test would let "nothing is ever expandable" through.
  it('is expandable when the failure HAS a message', () => {
    const { container } = render(<ToolCallList tools={failure('boom\nstack')} />)
    expect(container.querySelector('.msg-tool-row.clickable')).toBeTruthy()
    expect(container.querySelector('.msg-tool-caret')).toBeTruthy()
    expect(container.querySelector('.msg-tool-preview.err')?.textContent).toBe('error')
    fireEvent.click(container.querySelector('.msg-tool-row.clickable')!)
    expect(container.querySelector('.msg-tool-result.err')?.textContent).toContain('boom')
  })

  it('is NOT expandable when the failure has no message', () => {
    const { container } = render(<ToolCallList tools={failure('')} />)
    expect(container.querySelector('.msg-tool-row.clickable')).toBeNull()
    expect(container.querySelector('.msg-tool-caret')).toBeNull()
    // Not merely unexpanded — there is nothing to expand INTO. A click on the row
    // must not produce the empty block that made this a dead click.
    fireEvent.click(container.querySelector('.msg-tool-row')!)
    expect(container.querySelector('.msg-tool-result')).toBeNull()
  })

  // Whitespace is not a message. Found by review, and reachable rather than
  // theoretical: session.ccMessage.text keeps a `text` sub-block whenever it is not
  // the EMPTY string, so a result of "   " is served with length 3 and a
  // length-only check would expand into a blank <pre> — the same dead click one
  // shape over. Asserted for a success as well as a failure: the length-only check
  // had it on both.
  it('is NOT expandable when the message is only whitespace', () => {
    for (const isError of [true, false]) {
      const { container, unmount } = render(<ToolCallList
        tools={[{ id: 'w1', name: 'Bash', input: { command: 'true' }, result: { content: '  \n\t ', isError } }]}
      />)
      expect(container.querySelector('.msg-tool-row.clickable')).toBeNull()
      expect(container.querySelector('.msg-tool-caret')).toBeNull()
      fireEvent.click(container.querySelector('.msg-tool-row')!)
      expect(container.querySelector('.msg-tool-result')).toBeNull()
      unmount()
    }
  })

  it('still says error in BOTH cases — that is what may not regress', () => {
    // The whole reason the daemon serves an empty failure. summarizeToolResult
    // reads the flag, not the text, so losing the message must not lose the fact.
    for (const content of ['boom', '']) {
      const { container, unmount } = render(<ToolCallList tools={failure(content)} />)
      expect(container.querySelector('.msg-tool-preview.err')?.textContent).toBe('error')
      unmount()
    }
    expect(summarizeToolResult('Bash', { content: '', isError: true })).toBe('error')
  })
})

// tether#40 — parallel permission requests. PermissionQueue is exported + pure
// (the parent owns the POST + queue removal via onDecide/onDecideAll), so it
// tests without mounting ChatPane (which opens a WebTransport connection).
describe('PermissionQueue (tether#40)', () => {
  const reqs = (n: number): PermissionRequest[] =>
    Array.from({ length: n }, (_, i) => ({ id: `r${i}`, toolName: 'Read', input: { file_path: `f${i}.ts` } }))

  it('renders nothing when the queue is empty', () => {
    const { container } = render(<PermissionQueue requests={[]} onDecide={() => {}} onDecideAll={() => {}} />)
    expect(container.querySelector('.perm-queue')).toBeNull()
  })

  it('a single request shows just its block — no count header or bulk buttons (minimal)', () => {
    const { container } = render(<PermissionQueue requests={reqs(1)} onDecide={() => {}} onDecideAll={() => {}} />)
    expect(container.querySelectorAll('.perm-block').length).toBe(1)
    expect(container.querySelector('.perm-queue-head')).toBeNull()
    expect(screen.getByText('Read')).toBeTruthy()
  })

  it('two or more requests show a count header + Approve all / Deny all and one block each', () => {
    const { container } = render(<PermissionQueue requests={reqs(3)} onDecide={() => {}} onDecideAll={() => {}} />)
    expect(container.querySelectorAll('.perm-block').length).toBe(3)
    expect(container.querySelector('.perm-queue-count')?.textContent).toContain('3')
    expect(screen.getByText('Approve all')).toBeTruthy()
    expect(screen.getByText('Deny all')).toBeTruthy()
  })

  it('clicking a block Allow calls onDecide(id, true) for that request', () => {
    const onDecide = vi.fn()
    render(<PermissionQueue requests={reqs(2)} onDecide={onDecide} onDecideAll={() => {}} />)
    fireEvent.click(screen.getAllByText('Allow')[1]) // second block's Allow (id 'r1')
    expect(onDecide).toHaveBeenCalledWith('r1', true)
  })

  it('clicking a block Deny calls onDecide(id, false)', () => {
    const onDecide = vi.fn()
    render(<PermissionQueue requests={reqs(1)} onDecide={onDecide} onDecideAll={() => {}} />)
    fireEvent.click(screen.getByText('Deny'))
    expect(onDecide).toHaveBeenCalledWith('r0', false)
  })

  it('Approve all / Deny all call onDecideAll with the right flag', () => {
    const onDecideAll = vi.fn()
    render(<PermissionQueue requests={reqs(3)} onDecide={() => {}} onDecideAll={onDecideAll} />)
    fireEvent.click(screen.getByText('Approve all'))
    expect(onDecideAll).toHaveBeenCalledWith(true)
    fireEvent.click(screen.getByText('Deny all'))
    expect(onDecideAll).toHaveBeenCalledWith(false)
  })

  it('renders the tool name + input for each request', () => {
    const { container } = render(<PermissionQueue requests={[{ id: 'r0', toolName: 'Bash', input: { command: 'go test' } }]} onDecide={() => {}} onDecideAll={() => {}} />)
    expect(screen.getByText('Bash')).toBeTruthy()
    expect(container.querySelector('.perm-input')?.textContent).toContain('go test')
  })
})

// tether#40 — postDecide is the by-id decide POST reused by both a single block
// and the bulk Approve all / Deny all loop. Assert the endpoint contract (URL + body)
// so a route/shape drift is caught (review nit: this was the untested seam).
describe('postDecide (tether#40)', () => {
  afterEach(() => { vi.unstubAllGlobals() })

  it('POSTs the by-id decide endpoint with allow=true', async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200 })
    vi.stubGlobal('fetch', fetchMock)
    await postDecide('r7', true)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/agent/permission/r7/decide')
    expect(opts.method).toBe('POST')
    expect(JSON.parse(opts.body)).toEqual({ allow: true, remember: false })
  })

  it('carries allow=false for a deny', async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200 })
    vi.stubGlobal('fetch', fetchMock)
    await postDecide('r8', false)
    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/agent/permission/r8/decide')
    expect(JSON.parse(opts.body)).toEqual({ allow: false, remember: false })
  })
})

// tether#46 — multi-line composer. The composer is now a <textarea>: Enter
// sends, Shift+Enter inserts a newline, and it auto-grows to a cap. Both
// decisions are extracted as pure functions so they test without mounting
// ChatPane (which opens a WebTransport connection).
describe('shouldSendOnEnter (tether#46)', () => {
  const base = { key: 'Enter', shiftKey: false, isComposing: false, streaming: false, slashActive: false }
  it('plain Enter sends', () => {
    expect(shouldSendOnEnter(base)).toBe(true)
  })
  it('Shift+Enter does NOT send (inserts a newline instead)', () => {
    expect(shouldSendOnEnter({ ...base, shiftKey: true })).toBe(false)
  })
  it('Enter during IME composition does NOT send', () => {
    expect(shouldSendOnEnter({ ...base, isComposing: true })).toBe(false)
  })
  it('Enter while streaming does NOT send (button is Stop; tether#42)', () => {
    expect(shouldSendOnEnter({ ...base, streaming: true })).toBe(false)
  })
  it('Enter while the slash menu is open does NOT send (menu owns Enter)', () => {
    expect(shouldSendOnEnter({ ...base, slashActive: true })).toBe(false)
  })
  it('a non-Enter key never sends', () => {
    expect(shouldSendOnEnter({ ...base, key: 'a' })).toBe(false)
  })
})

describe('growHeight (tether#46)', () => {
  const opts = { lineHeightPx: 20, maxLines: 8, minLines: 1 }
  it('clamps a short content to at least one line', () => {
    // scrollHeight below one line still floors to minLines*lh.
    expect(growHeight(10, opts)).toEqual({ height: 20, scroll: false })
  })
  it('grows to fit content below the cap (no scroll)', () => {
    // 4 lines worth of content.
    expect(growHeight(80, opts)).toEqual({ height: 80, scroll: false })
  })
  it('caps at maxLines and switches on scrolling when content overflows', () => {
    // 12 lines of content, cap is 8 → height clamped to 160, scroll on.
    expect(growHeight(240, opts)).toEqual({ height: 160, scroll: true })
  })
  it('at exactly the cap does not scroll', () => {
    expect(growHeight(160, opts)).toEqual({ height: 160, scroll: false })
  })
  it('defaults minLines to 1 when omitted (matches production call site)', () => {
    // production omits minLines; the `?? 1` default must floor at one line.
    expect(growHeight(5, { lineHeightPx: 20, maxLines: 8 })).toEqual({ height: 20, scroll: false })
  })
})

// tether#47 — @-file mention. parseAtQuery finds the active @token at the caret;
// fuzzyRankFiles ranks the workspace file list against the query. Both pure.
describe('parseAtQuery (tether#47)', () => {
  it('finds an @token mid-text (preceded by whitespace)', () => {
    expect(parseAtQuery('see @foo', 8)).toEqual({ atPos: 4, query: 'foo' })
  })
  it('finds an @token at the very start', () => {
    expect(parseAtQuery('@foo', 4)).toEqual({ atPos: 0, query: 'foo' })
  })
  it('treats a bare @ as an empty query (show all)', () => {
    expect(parseAtQuery('@', 1)).toEqual({ atPos: 0, query: '' })
  })
  it('keeps path characters (slashes) in the query', () => {
    expect(parseAtQuery('@src/foo', 8)).toEqual({ atPos: 0, query: 'src/foo' })
  })
  it('returns null when a space separates the caret from the @ (token ended)', () => {
    expect(parseAtQuery('@foo bar', 8)).toBeNull()
  })
  it('returns null for an @ preceded by a non-space (e.g. an email)', () => {
    expect(parseAtQuery('a@b', 3)).toBeNull()
  })
  it('returns null when there is no @', () => {
    expect(parseAtQuery('hello world', 11)).toBeNull()
  })
  it('picks the nearest @ to the caret', () => {
    expect(parseAtQuery('foo @bar @ba', 12)).toEqual({ atPos: 9, query: 'ba' })
  })
})

describe('fuzzyRankFiles (tether#47)', () => {
  const files = ['src/foo.go', 'src/bar.go', 'README.md', 'foo/x.go']
  it('empty query returns the first `limit` files', () => {
    expect(fuzzyRankFiles(files, '', 2)).toEqual(['src/foo.go', 'src/bar.go'])
  })
  it('filters to subsequence matches (case-insensitive)', () => {
    const r = fuzzyRankFiles(files, 'FOO', 10)
    expect(r).toContain('src/foo.go')
    expect(r).toContain('foo/x.go')
    expect(r).not.toContain('README.md')
    expect(r).not.toContain('src/bar.go')
  })
  it('ranks a basename match above a directory-only match', () => {
    expect(fuzzyRankFiles(['a/foo.go', 'foo/x.go'], 'foo', 10)[0]).toBe('a/foo.go')
  })
  it('returns [] when nothing matches', () => {
    expect(fuzzyRankFiles(files, 'zzzq', 10)).toEqual([])
  })
  it('honors the limit', () => {
    expect(fuzzyRankFiles(files, 'o', 1)).toHaveLength(1)
  })
})

// tether#52 — shouldDeferFirstConnect decides whether ChatPane's mount effect
// waits for WorkspacePane's workspace list before opening the WebTransport
// connection. Only the sid-less (brand-new-session) path defers — see
// index.tsx's mount effect comment for why a remembered sid must never wait.
describe('shouldDeferFirstConnect (tether#52)', () => {
  it('defers when there is no remembered sid and workspaces have not loaded yet', () => {
    expect(shouldDeferFirstConnect({ hasLastSid: false, workspacesLoaded: false })).toBe(true)
  })

  it('does not defer once workspaces have loaded, even with no sid', () => {
    expect(shouldDeferFirstConnect({ hasLastSid: false, workspacesLoaded: true })).toBe(false)
  })

  // THE load-bearing negative: a remembered sid means the daemon already knows
  // that session's workspace (chatUrl.ts) — waiting on workspacesLoaded here
  // would add latency to the overwhelmingly common reconnect for no behavioral
  // effect, so a sid present must short-circuit to "don't defer" regardless.
  it('never defers when a last sid is remembered, regardless of workspacesLoaded', () => {
    expect(shouldDeferFirstConnect({ hasLastSid: true, workspacesLoaded: false })).toBe(false)
    expect(shouldDeferFirstConnect({ hasLastSid: true, workspacesLoaded: true })).toBe(false)
  })
})

// tether#63 — shouldReconnectAfterClose decides whether ChatPane's onClose
// schedules the reconnect ladder. Two independent stop conditions (unmounted,
// a terminal wire.ErrorPayload recorded as store.fatal); the ladder retries
// only when NEITHER holds. This is the fix for the silent-reconnect-loop bug:
// before it, a terminal refusal (e.g. unknown_workspace) still retried once a
// second forever, because the WebTransport handshake itself had succeeded —
// the refusal only arrives after (wt_chat.go sends it post-Upgrade).
describe('shouldReconnectAfterClose (tether#63)', () => {
  it('reconnects when neither unmounted nor fatal (the ordinary transient-drop case)', () => {
    expect(shouldReconnectAfterClose({ unmounted: false, fatal: false })).toBe(true)
  })

  it('does not reconnect once unmounted — nothing is left to hand the socket to', () => {
    expect(shouldReconnectAfterClose({ unmounted: true, fatal: false })).toBe(false)
  })

  // THE regression this slice fixes: a terminal refusal must stop the ladder
  // even though the pane is still mounted and would otherwise retry forever.
  it('does not reconnect when fatal, even though still mounted', () => {
    expect(shouldReconnectAfterClose({ unmounted: false, fatal: true })).toBe(false)
  })

  it('does not reconnect when both unmounted and fatal', () => {
    expect(shouldReconnectAfterClose({ unmounted: true, fatal: true })).toBe(false)
  })
})

// tether#63 — shouldRefundAttemptBudget is the structural half of the same fix:
// RECONNECT_MAX_ATTEMPTS was not a bound at all, because the budget was refunded
// on a WebTransport handshake and a refused connection completes a perfect
// handshake before dying. Measured on an unpatched build: 27 reconnects in 30s.
// Refunding only for a connection that lasted long enough to have been usable
// keeps the ladder bounded even when the refusal's REASON does not arrive (an
// older daemon, or a link slow enough to lose the envelope inside the daemon's
// 300ms drain grace) — in which case the pre-existing 5-attempt ladder ends it.
describe('shouldRefundAttemptBudget (tether#63)', () => {
  it('refunds for a connection that lasted long enough to be usable', () => {
    expect(shouldRefundAttemptBudget(2_000)).toBe(true)
    expect(shouldRefundAttemptBudget(60_000)).toBe(true)
  })

  // A refusal is torn down in well under a second — the daemon's own drain
  // grace is 300ms — so this is the case that must NOT hand its successor a
  // fresh budget.
  it('does not refund for a connection that died almost immediately', () => {
    expect(shouldRefundAttemptBudget(0)).toBe(false)
    expect(shouldRefundAttemptBudget(300)).toBe(false)
    expect(shouldRefundAttemptBudget(1_999)).toBe(false)
  })
})

// tether#88 — the autoscroll effect's dep. The effect needs a mounted pane and a
// scrollable element, so what is pinned here is the only part that can be: the
// signal that decides whether it re-runs at all.
// Only the FIRST test here discriminates: replacing the body with the old dep
// (the last message's text length) kills it and leaves the other three passing,
// which is what they are for — they pin the behaviour the rewrite had to keep,
// not the behaviour it added.
describe('transcriptTextLength (tether#88)', () => {
  const msg = (text: string) => ({ text })

  // THE case tether#88 created. Since sending a prompt no longer ends the running
  // turn, that turn's bubble stays ABOVE the user's new message and keeps growing
  // there. The old dep read only the LAST element, so it did not change and the
  // view stopped following the answer.
  it('changes when a message that is not the last one grows', () => {
    const before = [msg('half an answer'), msg('and another thing')]
    const after = [msg('half an answer and the rest'), msg('and another thing')]
    expect(transcriptTextLength(after)).not.toBe(transcriptTextLength(before))
  })

  // Unchanged behaviour, kept because it is the case that already worked and a
  // "sum everything" rewrite must not lose it.
  it('still changes when the last message grows', () => {
    expect(transcriptTextLength([msg('a'), msg('b')]))
      .not.toBe(transcriptTextLength([msg('a'), msg('bc')]))
  })

  // A re-emitted fenced block is replaced IN PLACE (store.ts's 'fenced' branch)
  // and carries text: '', so it must not read as growth — the same no-op it was
  // under the old dep, which this must not quietly turn into a scroll.
  it('does not change when a block message is replaced in place', () => {
    expect(transcriptTextLength([msg('answer'), msg('')]))
      .toBe(transcriptTextLength([msg('answer'), msg('')]))
  })

  it('is 0 for an empty transcript', () => {
    expect(transcriptTextLength([])).toBe(0)
  })
})

// tether#63 — FATAL_CODE_MESSAGES is PRESENTATION (the ladder's decision is the
// single `terminal` bit, never these strings), but a terminal code with no entry
// renders the generic fallback and quietly loses the specific explanation this
// slice exists to give. Tying the map to the generated wire constants means a
// renamed code fails here instead of silently degrading in the UI.
describe('FATAL_CODE_MESSAGES (tether#63)', () => {
  it('has a sentence for every code the daemon classifies as terminal', () => {
    for (const code of [
      ErrCodeUnknownWorkspace,
      ErrCodeNoWorkspaceRegistry,
      ErrCodeUnknownProvider,
      ErrCodeSessionOwned,
      // tether#101
      ErrCodeSessionHeldByBackgroundAgent,
    ]) {
      expect(FATAL_CODE_MESSAGES[code], `no sentence for ${code}`).toBeTruthy()
    }
  })

  it('has no entry for a retryable code — those never reach the card', () => {
    for (const code of [
      ErrCodeSpawnFailed, ErrCodeConnectionClosed, ErrCodeSessionUnconfirmed, ErrCodeAgent,
      // Retryable since tether#77 and never listed here until tether#101 added the
      // row; the map's own coverage test had the same gap the Go disposition table
      // did.
      ErrCodePromptUndelivered,
    ]) {
      expect(FATAL_CODE_MESSAGES[code]).toBeUndefined()
    }
  })

  // tether#101 — this code's sentence carries a requirement the other four do not,
  // and it is the whole reason the code exists rather than reusing one of them.
  //
  // The other terminal refusals are permanent: an unknown workspace stays unknown, a
  // session held by another device is never released. This one is TEMPORARY — the
  // background job finishes — so the sentence has to point the user forward. A card
  // that read "this connection was refused and cannot be retried automatically"
  // would be true about the ladder and useless about the conversation, which is the
  // shape of unhelpfulness this wi exists to remove one layer down.
  describe('the background-agent sentence', () => {
    const text = FATAL_CODE_MESSAGES[ErrCodeSessionHeldByBackgroundAgent] ?? ''

    it('says the conversation is in USE, not broken or gone', () => {
      expect(text).toMatch(/background agent/i)
      expect(text).toMatch(/using this conversation/i)
      // None of the words that would send the user away for good. The session is
      // fine; something else has it open.
      expect(text).not.toMatch(/\b(lost|gone|deleted|corrupt|no longer exists)\b/i)
    })

    // tether#104 REPLACES the two assertions that used to sit here. They pinned
    // "becomes resumable when that finishes", and that sentence was measured wrong
    // on the live deployment: the holder of the session that prompted tether#104
    // was alive, kind bg, cc status `idle`, three days old and hours since its last
    // status change. "When that finishes" is not a thing that was going to happen.
    //
    // The defect underneath the wording is the UNIT. cc refuses a `--resume` on
    // `kind && kind !== "interactive"` (ccregistry.go's ccInteractiveKind) — a
    // property of the PROCESS. A turn ending releases nothing; only the process
    // exiting does. So the sentence has to say which clock the user is waiting on,
    // and these two assertions pin that it does.
    it('names the process, not the turn, as what the user is waiting on', () => {
      expect(text).toMatch(/as long as that agent’s process/)
      expect(text).toMatch(/not just its current turn/)
    })

    // The corollary, and the reason this needs no busy/idle branch: cc's `status`
    // is not in the refusal rule, so the two statuses have the same remedy and the
    // same waiting condition. Saying so is what stops the next reader from adding
    // a distinction that changes no advice — and it is the clause that makes the
    // sentence true for the measured case rather than only for the imagined one.
    it('says an idle holder is no less of a holder than a busy one', () => {
      expect(text).toMatch(/an idle job holds this conversation exactly as firmly as a busy one/)
    })

    // Taking it over is now FIRST, and waiting is the qualified afterthought. On
    // the measured case the old order sent the user to wait for an event that was
    // not coming, past the one instruction that works whatever the holder is doing.
    it('offers taking it over before it offers waiting', () => {
      expect(text.indexOf('claude agents')).toBeGreaterThan(-1)
      expect(text.indexOf('Waiting')).toBeGreaterThan(-1)
      expect(text.indexOf('claude agents')).toBeLessThan(text.indexOf('Waiting'))
    })

    it('quotes the agent’s own two ways out', () => {
      // `claude agents` to take the session over, --fork-session to branch a copy.
      // Named because the agent writes them to its stderr, which reaches the
      // daemon's log and never the user. Naming --fork-session is not offering it:
      // tether deliberately forks on no path, because a fork mints a new id and
      // diverges instead of resuming.
      expect(text).toMatch(/claude agents/)
      expect(text).toMatch(/--fork-session/)
    })

    it('does not tell the user tether will fork or take over for them', () => {
      expect(text).not.toMatch(/tether will|we will|automatically (fork|take)/i)
    })
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#108 — the state line's classification, and the arrival rule.
//
// Both are pure and exported for the same reason as their five neighbours in
// index.tsx: the parts that need a mounted pane are tested in
// ChatPaneTranscript.test.tsx, and these are the parts that do not.
describe('heldActivityLine (tether#108)', () => {
  // One exact expected string per case, never a `toMatch` or a "not null": tether#102
  // measured a property assertion in this repo's own suite keeping a real mutant
  // alive, and the four answers here differ only by their wording.
  it('reports a turn in flight', () => {
    expect(heldActivityLine({ answered: true, state: SESSION_ACTIVITY_WORKING }))
      .toBe('Right now: a turn is in flight in that agent.')
    expect(heldActivityLine({ answered: true, state: SESSION_ACTIVITY_WORKING })).toBe(HELD_ACTIVITY_WORKING)
  })

  it('reports no turn in flight — and does NOT call it "between turns"', () => {
    expect(heldActivityLine({ answered: true, state: SESSION_ACTIVITY_IDLE }))
      .toBe('Right now: no turn is in flight in that agent.')
    // The daemon reports `idle` for cc's `waiting` (blocked on the user) and `shell`
    // (a shell task while the agent itself is idle), so "between turns" would be false
    // for both. session/activity.go says so at the constant; this pins the copy.
    expect(HELD_ACTIVITY_IDLE).not.toMatch(/between turns/i)
  })

  it('refuses to guess when the daemon said `held`', () => {
    // `held` is the fallback for a status this build cannot classify — a refusal to claim,
    // not a report about motion. Reading it as either of the other two is the mislabel
    // tether#103 exists to remove.
    //
    // One exact assertion, not that plus `not.toBe(WORKING)` and `not.toBe(IDLE)`: those
    // are tautologies once the value is pinned by identity, and a list of them reads like
    // three guards where there is one. What the `held` LITERAL maps to is pinned end to
    // end by the mounted test, which goes through fetchSessionActivity's known-set filter.
    expect(heldActivityLine({ answered: true, state: SESSION_ACTIVITY_HELD }))
      .toBe('Right now: tether cannot see whether a turn is in flight in that agent.')
    expect(HELD_ACTIVITY_UNKNOWN)
      .toBe('Right now: tether cannot see whether a turn is in flight in that agent.')
  })

  it('reads ABSENCE as the hold having ended, and points at the button', () => {
    // The fourth answer, and the most common one: the activity map holds every sid
    // something live is holding, so a sid that is not in it is not held. Absence is
    // sound here specifically because the refusal on screen and this map come from the
    // same cc registry instance through two filters and this one is the wider — see
    // heldActivityLine's doc.
    expect(heldActivityLine({ answered: true, state: undefined }))
      .toBe('Right now: nothing live is holding this conversation — Check again should open it.')
    // It names the button by the word the button actually has, so a rename of one has
    // to change the other.
    expect(HELD_ACTIVITY_GONE).toContain('Check again')
    // And it reports what tether can SEE, not that the process exited. cc's liveness check
    // is pid + /proc start token and reads "not live" for an unreadable /proc too, so the
    // stronger sentence would be a claim this pane cannot back.
    expect(HELD_ACTIVITY_GONE).not.toMatch(/exited|has ended|that process/i)
  })

  it('says NOTHING until the daemon has answered once', () => {
    // Without this branch the line would announce that the hold had ended on every
    // mount, for one round trip, before anything had been asked — a false claim about
    // another process, made by default.
    expect(heldActivityLine({ answered: false, state: undefined })).toBeNull()
    // …and `answered: false` wins even if a state somehow travelled with it, because
    // the flag is about whether this build has heard from the daemon at all.
    expect(heldActivityLine({ answered: false, state: SESSION_ACTIVITY_WORKING })).toBeNull()
  })

  it('degrades an unrecognised state to "cannot tell", NOT to "nothing is holding it"', () => {
    // A daemon newer than this bundle. This case is the reason the absence test comes
    // first and the fallback is `held` rather than a `switch` default: an unknown state
    // string means the daemon reported SOMETHING for that sid, so "nothing live is
    // holding this conversation any more" would be false — and it is the one answer a
    // reader acts on. Asserted as the exact sentence, because the first version of this
    // test only checked "not working and not idle", which the wrong answer satisfies.
    expect(heldActivityLine({ answered: true, state: 'sprinting' as never }))
      .toBe('Right now: tether cannot see whether a turn is in flight in that agent.')
    // fetchSessionActivity drops unknown strings, so the poller cannot produce this —
    // pinned anyway, because "unreachable" is a property of another module.
  })
})

describe('trailingArrivals (tether#108)', () => {
  const msgs = (...ids: string[]) => ids.map(id => ({ id }))

  it('returns the trailing run of ids that were not on screen before', () => {
    expect(trailingArrivals(new Set(['a', 'b']), msgs('a', 'b', 'c', 'd'))).toEqual(['c', 'd'])
  })

  it('returns nothing when the last message is one already on screen', () => {
    expect(trailingArrivals(new Set(['a', 'b', 'c']), msgs('a', 'b', 'c'))).toEqual([])
  })

  it('IGNORES new ids that are not at the end — which is what excludes a prepend', () => {
    // THE property that makes tether#107's "load earlier messages" trace-free without a
    // flag. Older pages arrive at the FRONT, so the walk from the end stops at the first
    // id that was already there. A rule written as "every id that is new" would flash 25
    // bubbles the reader deliberately asked for, saying they had just landed.
    expect(trailingArrivals(new Set(['c', 'd']), msgs('a', 'b', 'c', 'd'))).toEqual([])
    // Mixed: a prepend AND an arrival in one commit still traces only the arrival.
    expect(trailingArrivals(new Set(['c', 'd']), msgs('a', 'b', 'c', 'd', 'e'))).toEqual(['e'])
  })

  it('returns them in transcript order, oldest first', () => {
    // The walk is backwards; the answer must not be. Order is not cosmetic here — the
    // caller builds a Set, but a reversed array is the sort of thing that reads as
    // working right up until something iterates it.
    expect(trailingArrivals(new Set(['a']), msgs('a', 'b', 'c', 'd'))).toEqual(['b', 'c', 'd'])
  })

  it('treats an empty previous set as everything being new', () => {
    // Honest, and the reason the CALLER owns the "first sight" decision: only it knows
    // whether the empty set means "this sid had no messages" or "we have never looked",
    // and flashing an entire transcript on open is the failure mode.
    expect(trailingArrivals(new Set(), msgs('a', 'b'))).toEqual(['a', 'b'])
    expect(trailingArrivals(new Set(['a']), msgs())).toEqual([])
  })
})
