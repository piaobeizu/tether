// Markdown — lazy-loaded renderer for the Canvas middle pane (tether#21).
//
// react-markdown escapes any embedded raw HTML by default (it never renders
// it), which is what keeps this XSS-safe by construction. Do NOT add
// rehype-raw or any `rehypePlugins` that allow raw HTML, and do NOT reach for
// dangerouslySetInnerHTML here — that would defeat the escaping guarantee.
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

export default function Markdown({ text }: { text: string }) {
  return (
    <div className="md-body">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  )
}
