import type { CSSProperties } from 'react'

export type IconName =
  | 'tether' | 'chevron' | 'chev-down' | 'folder' | 'folder-open'
  | 'search' | 'plus' | 'settings' | 'phone' | 'bolt' | 'arrow-up'
  | 'x' | 'back' | 'menu' | 'ellipsis'

interface IconProps {
  name: IconName
  size?: number
  style?: CSSProperties
}

export function Icon({ name, size = 16, style }: IconProps) {
  const shared = {
    width: size,
    height: size,
    viewBox: '0 0 16 16',
    fill: 'none',
    stroke: 'currentColor',
    strokeWidth: 1.5,
    strokeLinecap: 'round' as const,
    strokeLinejoin: 'round' as const,
    style,
    'aria-hidden': true,
  }

  switch (name) {
    case 'tether':
      return <svg {...shared}><path d="M2 3h12M8 3v10" strokeWidth={2.5}/></svg>
    case 'chevron':
      return <svg {...shared}><path d="M6 3l5 5-5 5"/></svg>
    case 'chev-down':
      return <svg {...shared}><path d="M3 6l5 5 5-5"/></svg>
    case 'folder':
      return <svg {...shared}><path d="M1 5c0-1.1.9-2 2-2h3.2L8 5h5a1 1 0 011 1v6a1 1 0 01-1 1H3a2 2 0 01-2-2V5z"/></svg>
    case 'folder-open':
      return <svg {...shared}><path d="M1 7c0-1.1.9-2 2-2h3.2L8 5h5l1 6H2L1 7z"/></svg>
    case 'search':
      return <svg {...shared}><circle cx="7" cy="7" r="4.5"/><path d="M10.5 10.5L14 14"/></svg>
    case 'plus':
      return <svg {...shared}><path d="M8 2v12M2 8h12"/></svg>
    case 'settings':
      return <svg {...shared}><circle cx="8" cy="8" r="2.5"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M3.2 12.8l1.4-1.4M11.4 4.6l1.4-1.4"/></svg>
    case 'phone':
      return <svg {...shared}><path d="M5 2h6a1 1 0 011 1v10a1 1 0 01-1 1H5a1 1 0 01-1-1V3a1 1 0 011-1z"/><circle cx="8" cy="11.5" r=".5" fill="currentColor"/></svg>
    case 'bolt':
      return <svg {...shared}><path d="M9 2L5 9h4l-2 5 8-7h-4z"/></svg>
    case 'arrow-up':
      return <svg {...shared}><path d="M8 13V3M4 7l4-4 4 4"/></svg>
    case 'x':
      return <svg {...shared}><path d="M4 4l8 8M12 4L4 12"/></svg>
    case 'back':
      return <svg {...shared}><path d="M11 3L5 8l6 5M5 8h9"/></svg>
    case 'menu':
      return <svg {...shared}><path d="M3 5h10M3 8h10M3 11h10"/></svg>
    case 'ellipsis':
      return (
        <svg {...shared} stroke="none">
          <circle cx="4" cy="8" r="1.3" fill="currentColor"/>
          <circle cx="8" cy="8" r="1.3" fill="currentColor"/>
          <circle cx="12" cy="8" r="1.3" fill="currentColor"/>
        </svg>
      )
  }
}
