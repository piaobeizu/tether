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
// object — so tailwind would then go looking for a tailwind.config.js of its own,
// find none beside it, and fall back to its defaults. Defaults mean no vendored
// design tokens, no vendored content globs, and no error: a build that is green
// and empty. Passing the resolved config in is what closes that.
import autoprefixer from 'autoprefixer'
import tailwindcss from 'tailwindcss'

import tetherTailwindConfig from './tailwind.config.mjs'

export default {
  plugins: [tailwindcss(tetherTailwindConfig), autoprefixer()],
}
