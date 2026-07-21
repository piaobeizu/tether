import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render } from '@testing-library/react'
import Markdown from './Markdown'

// tether#41 — fenced code blocks get syntax highlighting via rehype-highlight:
// the plugin adds `hljs` + keeps the `language-<lang>` class on the <code>, and
// wraps tokens in `.hljs-*` spans (colored theme-aware in index.css). It does
// NOT enable raw HTML, so the XSS-safe-by-construction guarantee still holds.
afterEach(() => cleanup())

describe('Markdown code highlighting (tether#41)', () => {
  it('highlights a fenced code block with a language (hljs class + token spans)', () => {
    const { container } = render(<Markdown text={'```go\nfunc main() { return }\n```'} />)
    const code = container.querySelector('code.hljs')
    expect(code).toBeTruthy()
    expect(code?.className).toContain('language-go')
    // at least one recognized token became a highlighted span
    expect(container.querySelectorAll('.hljs-keyword, .hljs-title, .hljs-built_in, .hljs-type, .hljs-string').length).toBeGreaterThan(0)
  })

  it('does NOT force-highlight an unlabeled block (detect:false → no token spans)', () => {
    const { container } = render(<Markdown text={'```\njust plain text, no language\n```'} />)
    expect(container.querySelector('pre code')).toBeTruthy() // still a code block
    expect(container.querySelectorAll('.hljs-keyword, .hljs-string, .hljs-title, .hljs-number').length).toBe(0)
  })

  it('leaves inline code untouched (block-only highlighting)', () => {
    const { container } = render(<Markdown text={'use `const x = 1` inline'} />)
    expect(container.querySelector('code')?.textContent).toBe('const x = 1')
    expect(container.querySelector('pre')).toBeNull() // inline, not a block
  })

  it('still escapes raw HTML (XSS-safe by construction preserved)', () => {
    const { container } = render(<Markdown text={'<img src=x onerror="alert(1)"> and text'} />)
    expect(container.querySelector('img')).toBeNull()          // raw HTML not rendered
    expect(container.textContent).toContain('<img')            // shown as escaped text
  })

  it('still renders ordinary markdown (bold, lists) alongside highlighting', () => {
    const { container } = render(<Markdown text={'see **bold**\n\n- one\n- two'} />)
    expect(container.querySelector('strong')?.textContent).toBe('bold')
    expect(container.querySelectorAll('li').length).toBe(2)
  })
})
