import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { AnswerBody, AnswerMeta, ThinkingBlock, ToolCallList, fmtThinkMs, summarizeToolInput, summarizeToolResult, truncateResult, shouldSendOnEnter, growHeight, parseAtQuery, fuzzyRankFiles } from './index'
import { PermissionQueue, postDecide } from '../../fenced-blocks/PermissionBlock'
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
    expect(screen.getByText('思考中…')).toBeTruthy()
    expect(screen.getByText('pondering the plan')).toBeTruthy()
  })

  it('collapses to a "思考 Xs" summary once no longer live, hiding the text', () => {
    render(<ThinkingBlock thinking="secret reasoning" thinkingMs={8000} live={false} expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('思考 8s')).toBeTruthy()
    expect(screen.queryByText('secret reasoning')).toBeNull()
  })

  it('a collapsed thinking-only turn (no duration) still shows a bare "思考" summary', () => {
    render(<ThinkingBlock thinking="only thought" live={false} expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('思考')).toBeTruthy()
    expect(screen.queryByText('only thought')).toBeNull()
  })

  it('shows the thinking text when expanded', () => {
    render(<ThinkingBlock thinking="secret reasoning" thinkingMs={8000} live={false} expanded onToggle={() => {}} />)
    expect(screen.getByText('secret reasoning')).toBeTruthy()
  })

  it('clicking the collapsed summary calls onToggle', () => {
    const onToggle = vi.fn()
    render(<ThinkingBlock thinking="x" thinkingMs={8000} live={false} expanded={false} onToggle={onToggle} />)
    fireEvent.click(screen.getByText('思考 8s'))
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

  it('folds beyond the threshold into a "用了 N 个工具" summary, hiding rows until expanded', () => {
    const { container } = render(<ToolCallList tools={mk(8)} />)
    expect(container.querySelectorAll('.msg-tool-row').length).toBe(0) // collapsed by default
    const fold = container.querySelector('.msg-tool-fold')!
    expect(fold.textContent).toContain('用了 8 个工具')
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
    expect(summarizeToolResult('Bash', { content: 'boom', isError: true })).toBe('出错')
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
    expect(out.endsWith('…（已截断）')).toBe(true)
  })
  it('clamps by char count with a marker', () => {
    const out = truncateResult('x'.repeat(3000))
    expect(out.length).toBeLessThan(3000)
    expect(out.endsWith('…（已截断）')).toBe(true)
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
    expect(container.querySelector('.msg-tool-preview.err')?.textContent).toBe('出错')
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

  it('two or more requests show a count header + 全部批准/全部拒绝 and one block each', () => {
    const { container } = render(<PermissionQueue requests={reqs(3)} onDecide={() => {}} onDecideAll={() => {}} />)
    expect(container.querySelectorAll('.perm-block').length).toBe(3)
    expect(container.querySelector('.perm-queue-count')?.textContent).toContain('3')
    expect(screen.getByText('全部批准')).toBeTruthy()
    expect(screen.getByText('全部拒绝')).toBeTruthy()
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

  it('全部批准 / 全部拒绝 call onDecideAll with the right flag', () => {
    const onDecideAll = vi.fn()
    render(<PermissionQueue requests={reqs(3)} onDecide={() => {}} onDecideAll={onDecideAll} />)
    fireEvent.click(screen.getByText('全部批准'))
    expect(onDecideAll).toHaveBeenCalledWith(true)
    fireEvent.click(screen.getByText('全部拒绝'))
    expect(onDecideAll).toHaveBeenCalledWith(false)
  })

  it('renders the tool name + input for each request', () => {
    const { container } = render(<PermissionQueue requests={[{ id: 'r0', toolName: 'Bash', input: { command: 'go test' } }]} onDecide={() => {}} onDecideAll={() => {}} />)
    expect(screen.getByText('Bash')).toBeTruthy()
    expect(container.querySelector('.perm-input')?.textContent).toContain('go test')
  })
})

// tether#40 — postDecide is the by-id decide POST reused by both a single block
// and the bulk 全部批准/全部拒绝 loop. Assert the endpoint contract (URL + body)
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
