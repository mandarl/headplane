// MARK: Phase 0 interim — server-action re-export shim.
//
// The login route module (`page.tsx`) is shared between the SPA build and the
// SSR-proxy build. React Router's SPA build rejects route modules that
// textually export server-only names (`action`, `loader`, `headers`), so
// `page.tsx` re-exports this module with `export *` — the star export does
// not surface the name `action` to static analysis, but both builds still
// resolve it: the SSR server serves it, and the SPA client tree-shakes it
// away. (Re-exporting `{ loginAction as action }` directly from `page.tsx`
// would trip the validator, which is why this indirection exists.)
export { loginAction as action } from "./action";
