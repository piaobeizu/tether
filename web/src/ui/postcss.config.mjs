// The PostCSS config tether actually builds with. tether#194.
//
// web/vite.config.ts points `css.postcss` at this directory, so postcss-load-config
// finds this file and `import()`s it from disk. See the header of
// ./tailwind.config.mjs for why that route is load-bearing rather than a detail.
//
// Same two plugins in the same order as the vendored
// web/src/vendor/cloudcli/postcss.config.js, which cannot be used directly: that
// file names its plugins as the strings "tailwindcss" and "autoprefixer", and
// string form makes postcss resolve them itself and hand tailwind *no* config
// object. Tailwind then looks for a tailwind.config.js of its own, finds none at
// web/, and resolves its built-in defaults.
//
// Measured, rather than assumed — an earlier version of this comment claimed that
// state was "green and empty", and it is not. `pnpm build` FAILS, loudly:
//
//   [vite:css] [postcss] web/src/vendor/cloudcli/src/index.css:14:1:
//     The `border-border` class does not exist. If `border-border` is a custom
//     class, make sure it is defined within a `@layer` directive.
//
// because the vendored stylesheet `@apply`s `border-border`, and that class only
// exists by way of the vendored config's `colors.border`. Worth being precise
// about why that is good news and how far it goes: the loudness comes from the
// vendored stylesheet happening to use `@apply` on a themed class, not from
// anything structural. A vendored stylesheet without such an `@apply` would take
// this same wrong turn in silence. So passing the resolved config in is still what
// closes it; the error above is a symptom that happens to be visible, not the gate.
//
// The failure mode that IS silent here is losing the postcss config altogether —
// see ../../postcss.config.mjs, and scripts/check-tailwind-emitted.sh for what
// catches it.
import autoprefixer from 'autoprefixer'
import tailwindcss from 'tailwindcss'

import tetherTailwindConfig from './tailwind.config.mjs'

export default {
  plugins: [tailwindcss(tetherTailwindConfig), autoprefixer()],
}
