# Local PC/SC reset addition

Source: `github.com/damonto/uicc-go`, pinned version
`v0.0.0-20260629073618-7ddada6bb13e` (commit `7ddada6bb13e`).
Copied from the verified Go module cache; the upstream license is retained.

Switch adds `ccid.ResetCard` in `ccid/reset.go`. It uses the same private
`newContext` / `acquirePCSC` / `releasePCSC` lifecycle as upstream `Open` and
`ListReaders`. An overlapping reader enumeration cannot unload the library
while an exclusive reset still owns its context. The card disconnects before
the context and library reference are released, including error/cancel paths.

The existing initialization and finalization calls in `ccid.go` are routed
through package-private functions with identical production defaults to
allow tests without a real PC/SC library or card. Existing public APIs and
their normal behavior are unchanged.
