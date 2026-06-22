package io.tipsy.abconfig.deve2e;

/**
 * Accumulates PASS / FAIL counts for the assertion loop and flags a tolerated
 * gRPC connect/init failure as {@code grpcDegraded}. Mirrors the Go driver's
 * {@code results} struct: a degraded gRPC transport is a visible non-success
 * (the process still exits non-zero) but does not crash the HTTP run.
 */
final class Results {

    int passed;
    int failed;
    boolean grpcDegraded;

    void pass(String fmt, Object... args) {
        passed++;
        System.out.println("PASS  " + String.format(fmt, args));
    }

    void fail(String fmt, Object... args) {
        failed++;
        System.out.println("FAIL  " + String.format(fmt, args));
    }
}
