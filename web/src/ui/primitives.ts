// tether's barrel for the vendored CloudCLI UI primitives. tether#194.
//
// Application code imports primitives from here and never reaches into
// web/src/vendor/cloudcli/ directly. That is what keeps the wrap layer a real
// seam: the day a primitive needs a tether default bound, or needs replacing
// outright, this file changes and its callers do not.
//
// Upstream has its own barrel at src/shared/view/ui/index.ts. It is deliberately
// NOT vendored — it re-exports every sibling in that directory, so carrying it
// would mean either carrying all of them or shipping a file with imports that do
// not resolve. This is the tether-side equivalent, and it lists only what is
// actually vendored.
//
// Deliberately no `import './index.css'` here. The stylesheet's entry point is
// web/index.html, so the built CSS asset does not depend on which JS module
// happened to be imported first, and a component import does not silently drag in
// a 1,000-line stylesheet.
export { Button, buttonVariants } from '../vendor/cloudcli/src/shared/view/ui/Button'
export { Input } from '../vendor/cloudcli/src/shared/view/ui/Input'
