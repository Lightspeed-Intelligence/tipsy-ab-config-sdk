package io.github.lightspeedintelligence.auth;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/**
 * Immutable description of a token to mint. Construct via {@link #builder()}.
 *
 * <p>Counterpart of Go {@code tipsyauth.IssueOptions}. {@code ttl} is required
 * and must be {@code > 0} (validated at {@link JwtSigner#issue(IssueOptions)}
 * time, matching the Go signer). {@code issuedAt} may be {@code null}, in which
 * case the signer substitutes {@code Instant.now()}.
 *
 * <p>{@code roles} and {@code namespaces} are never {@code null}: the builder
 * defensively copies the supplied lists (or substitutes an empty list when
 * {@code null}) and exposes them as unmodifiable lists. This guarantees the
 * minted JWT always carries {@code "roles"} / {@code "namespaces"} as JSON
 * arrays (empty {@code []} when no values), matching Go's
 * {@code append([]string{}, ...)} behaviour.
 */
public final class IssueOptions {

    private final String subject;
    private final List<String> roles;
    private final List<String> namespaces;
    private final Duration ttl;
    private final Instant issuedAt;

    private IssueOptions(Builder b) {
        this.subject = b.subject;
        this.roles = copyOf(b.roles);
        this.namespaces = copyOf(b.namespaces);
        this.ttl = b.ttl;
        this.issuedAt = b.issuedAt;
    }

    private static List<String> copyOf(List<String> src) {
        if (src == null || src.isEmpty()) {
            return Collections.emptyList();
        }
        return Collections.unmodifiableList(new ArrayList<>(src));
    }

    /** The {@code sub} claim; may be {@code null} or empty. */
    public String subject() {
        return subject;
    }

    /** The {@code roles} claim values; never {@code null} (may be empty). */
    public List<String> roles() {
        return roles;
    }

    /** The {@code namespaces} claim values; never {@code null} (may be empty). */
    public List<String> namespaces() {
        return namespaces;
    }

    /** Token lifetime; required to be {@code > 0} (validated at issue time). */
    public Duration ttl() {
        return ttl;
    }

    /** Issue instant; may be {@code null} (signer substitutes {@code Instant.now()}). */
    public Instant issuedAt() {
        return issuedAt;
    }

    /** Creates a new builder. */
    public static Builder builder() {
        return new Builder();
    }

    /** Mutable builder for {@link IssueOptions}. Not thread-safe. */
    public static final class Builder {
        private String subject;
        private List<String> roles;
        private List<String> namespaces;
        private Duration ttl;
        private Instant issuedAt;

        private Builder() {
        }

        /** Sets the {@code sub} claim. */
        public Builder subject(String subject) {
            this.subject = subject;
            return this;
        }

        /** Sets the {@code roles} claim values ({@code null} treated as empty). */
        public Builder roles(List<String> roles) {
            this.roles = roles;
            return this;
        }

        /** Sets the {@code namespaces} claim values ({@code null} treated as empty). */
        public Builder namespaces(List<String> namespaces) {
            this.namespaces = namespaces;
            return this;
        }

        /** Sets the token lifetime; required and must be {@code > 0}. */
        public Builder ttl(Duration ttl) {
            this.ttl = ttl;
            return this;
        }

        /** Sets the issue instant; {@code null} → {@code Instant.now()} at issue time. */
        public Builder issuedAt(Instant issuedAt) {
            this.issuedAt = issuedAt;
            return this;
        }

        /** Builds an immutable {@link IssueOptions}. */
        public IssueOptions build() {
            return new IssueOptions(this);
        }
    }
}
