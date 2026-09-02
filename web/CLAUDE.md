@AGENTS.md

## Testing gotcha (2026-09-03)

`@azex/ledger-react` has gates that read `dist/` (`test/build-artifacts.test.ts`,
`test/styles.test.ts`). Run `npm run -w @azex/ledger-react build` before
`npm test`, or seven tests fail with a stale or missing `dist/` even though the
source is correct. CI does build first; locally it is on you.
