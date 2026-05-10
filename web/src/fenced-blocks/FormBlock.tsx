import type { FencedBlock } from '../lib/wire.gen'

interface Props { block: FencedBlock }

export function FormBlock({ block }: Props) {
  return (
    <div style={{ background: '#1a1a1a', padding: 8, borderRadius: 4, fontSize: 11 }}>
      [form:{block.skill}] {block.content}
    </div>
  )
}
