/**
 * Tipsy AB-config HS256 JWT signing utility.
 *
 * <p>Provides {@link io.tipsy.auth.JwtSigner} and
 * {@link io.tipsy.auth.IssueOptions} for minting HS256 JWTs accepted by the
 * ab-config services. Signing only; verification is the server's job. See
 * {@link io.tipsy.auth.JwtSigner} for the compatibility contract with the Go
 * signer and the server's internal auth boundary.
 */
package io.tipsy.auth;
