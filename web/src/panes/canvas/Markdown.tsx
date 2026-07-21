// Markdown — shared renderer for the Canvas middle pane (tether#21) and, since
// tether#35, the chat AI answer/thinking. tether#41 adds syntax highlighting;
// tether#42 adds a per-code-block copy button.
//
// react-markdown escapes any embedded raw HTML by default (it never renders
// it), which is what keeps this XSS-safe by construction. Do NOT add
// rehype-raw or any `rehypePlugins` that allow raw HTML, and do NOT reach for
// dangerouslySetInnerHTML here — that would defeat the escaping guarantee.
//
// rehype-highlight is the ALLOWED exception (tether#41): it only walks the
// already-parsed `<pre><code>` nodes and wraps tokens in `.hljs-*` spans — it
// never enables or injects raw HTML, so the escaping guarantee is preserved.
// `detect: false` means only fenced blocks that declare a language (```go) get
// highlighted; unlabeled blocks stay plain (no guessing). Token colors are
// theme-aware in index.css (`.md-body .hljs-*`), so they follow light/dark.
import { useRef, type ComponentPropsWithoutRef } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import { CopyButton } from '../../lib/CopyButton'

// Pre wraps each fenced code block with a copy button (tether#42). It copies the
// block's rendered text (ref.textContent) — robust to the `.hljs-*` token spans
// rehype-highlight injects (tether#41) — and the button sits OUTSIDE <pre> so it
// never becomes part of the copied text. Shared → chat + canvas both get it.
function Pre(props: ComponentPropsWithoutRef<'pre'> & { node?: unknown }) {
  const ref = useRef<HTMLPreElement>(null)
  const { node, ...preProps } = props
  void node // react-markdown passes the hast node; keep it off the DOM <pre>.
  return (
    <div className="md-pre">
      <CopyButton className="md-code-copy" label="Copy code" getText={() => ref.current?.textContent ?? ''} />
      <pre ref={ref} {...preProps} />
    </div>
  )
}

export default function Markdown({ text }: { text: string }) {
  return (
    <div className="md-body">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: false }]]}
        components={{ pre: Pre }}
      >{text}</ReactMarkdown>
    </div>
  )
}
