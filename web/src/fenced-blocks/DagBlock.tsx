import type { FencedBlock } from '../lib/wire.gen'

interface Props { block: FencedBlock }

export function DagBlock({ block }: Props) {
  return (
    <pre style={{ background: '#1a1a1a', padding: 8, borderRadius: 4, fontSize: 11, overflow: 'auto' }}>
      [dag:{block.skill}] {block.content}
    </pre>
  )
}
