/**
 * Tipsy AB-config HS256 JWT signing utility.
 *
 * <p>Provides {@link io.github.lightspeedintelligence.auth.JwtSigner} and
 * {@link io.github.lightspeedintelligence.auth.IssueOptions} for minting HS256 JWTs accepted by the
 * ab-config services. Signing only; verification is the server's job. See
 * {@link io.github.lightspeedintelligence.auth.JwtSigner} for the compatibility contract with the Go
 * signer and the server's internal auth boundary.
 */
package io.github.lightspeedintelligence.auth;
