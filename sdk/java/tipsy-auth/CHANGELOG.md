# Changelog

All notable changes to the Tipsy AB-config Java auth module
(`io.github.lightspeed-intelligence:tipsy-auth`) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-06-22

First release. Counterpart of the Go `tipsyauth` signer; completely
independent of the main SDK / proto / gRPC (depends only on jjwt).

### Added

- `JwtSigner.create(secret)` / `issue(IssueOptions)` — mints HS256 JWTs whose
  claims (`roles`, `namespaces`, `sub`, `iat`, `exp`) match the server's
  internal signer, so tokens are accepted by the server's verifier when both
  sides share the same `TIPSY_SERVICE_SECRET`. `iat` / `exp` are emitted as
  unix seconds (NumericDate); there is no `nbf` / `iss` / `aud`. `roles` /
  `namespaces` are always emitted as JSON arrays (empty `[]` when unset).
- `IssueOptions` (+ `IssueOptions.Builder`) — `subject`, `roles`,
  `namespaces`, `ttl` (required, must be `> 0`), optional `issuedAt`
  (defaults to `Instant.now()` at issue time).
- Arbitrary-length HMAC secrets are accepted (matching Go / golang-jwt): a
  custom `HS256` `MacAlgorithm` computes the identical HMAC-SHA256 without
  jjwt's JWA minimum-key-length validation.

### Notes

- Verification (signature + `exp` / `iat` + namespace authorization) is the
  server's responsibility and is intentionally NOT implemented here.
