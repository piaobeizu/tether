import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { AnswerBody, ThinkingBlock, fmtThinkMs } from './index'

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
