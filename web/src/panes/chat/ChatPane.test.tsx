import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { AnswerBody, AnswerMeta, ThinkingBlock, ToolCallList, fmtThinkMs, summarizeToolInput, summarizeToolResult, truncateResult } from './index'
import type { ToolCall } from '../../lib/store'

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
