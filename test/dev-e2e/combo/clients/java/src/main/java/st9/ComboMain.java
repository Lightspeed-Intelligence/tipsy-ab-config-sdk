package st9;

import io.github.lightspeedintelligence.abconfig.Config;
import io.github.lightspeedintelligence.abconfig.TipsyAbConfigClient;
import io.github.lightspeedintelligence.abconfig.Transport;

import java.util.List;

/**
 * ST9 combo data-plane driver for the Java SDK (assertion #2). Mirrors the Go and
 * Python drivers.
 *
 * <p>The fixture makes frozen != live on purpose (group frozen at v1 while the
 * full release moved to v2); were they equal, the assertion would pass either way
 * and prove nothing.
 *
 * <p>It also asserts the abtestFallback DELTA is zero: a correct-looking value
 * could otherwise have been delivered by the ab-&gt;full fallback arm rather than
 * the abtest main path. Delta rather than absolute, so unrelated startup /
 * other-key fallbacks cannot make it red for the wrong reason.
 */
public final class ComboMain {
    private static final String NS = "st9_combo";

    private record Case(String uid, String key, String want, String why) {}

    private static final List<Case> CASES = List.of(
            new Case("st9-probe-9", "managed_a", "LIVE_v1", "held by h1 -> frozen v1"),
            new Case("st9-probe-24", "managed_a", "LIVE_v1", "held by h1 -> frozen v1"),
            new Case("st9-probe-4", "managed_a", "LIVE_v1", "held by o1 -> frozen v1"),
            new Case("st9-probe-10", "managed_a", "LIVE_v1", "held by o1 -> frozen v1"),
            // These two take the LIVE value because no combo group holds managed_a
            // FOR THEM — not because they are outside every combo. st9-probe-1 is in
            // c2mig's o_wide group (holding mig_key only), which makes it the stronger
            // case: "in a combo group, but that group does not hold this key => live
            // value". See fixture.md section 10 for the full membership table;
            // claiming managed_a into c2mig or c3zero would turn these red.
            // st9-probe-0 is deliberately NOT used here: assertion #5 pins it into
            // the holdout-opt domain, making its expected value ambiguous.
            new Case("st9-probe-1", "managed_a", "LIVE_v2", "in c2mig/o_wide; group does not hold this key -> live full v2"),
            new Case("st9-probe-2", "managed_a", "LIVE_v2", "in E_mig/gA; group does not hold this key -> live full v2"),
            new Case("st9-probe-9", "unheld_key", "UNHELD_v1", "NOT held -> live full"),
            new Case("st9-probe-4", "unheld_key", "UNHELD_v1", "NOT held -> live full"),
            new Case("st9-probe-1", "unheld_key", "UNHELD_v1", "in c2mig/o_wide; no group holds it -> live full"));

    private static int passed = 0;
    private static int failed = 0;

    public static void main(String[] args) {
        String transport = "both";
        String httpBase = "http://localhost:8081";
        String grpcTarget = "grpc://localhost:50052";
        for (int i = 0; i < args.length - 1; i++) {
            if (args[i].equals("--transport")) transport = args[i + 1];
            if (args[i].equals("--http")) httpBase = args[i + 1];
            if (args[i].equals("--grpc")) grpcTarget = args[i + 1];
        }
        String token = System.getenv("AB_CONFIG_TOKEN");
        if (token == null || token.isEmpty()) {
            System.err.println("FATAL: AB_CONFIG_TOKEN required");
            System.exit(2);
        }

        if (transport.equals("both") || transport.equals("http")) {
            run("java_http", Config.builder().namespaces(List.of(NS))
                    .configServiceAddr(httpBase).abtestServiceAddr(httpBase)
                    .token(token).transport(Transport.HTTP).startupFailOpen(true).build());
        }
        if (transport.equals("both") || transport.equals("grpc")) {
            run("java_grpc", Config.builder().namespaces(List.of(NS))
                    .configServiceAddr(grpcTarget).abtestServiceAddr(grpcTarget)
                    .token(token).transport(Transport.GRPC).startupFailOpen(true).build());
        }

        int total = passed + failed;
        System.out.println("\n----");
        System.out.printf("SUMMARY: pass=%d fail=%d of %d checks%n", passed, failed, total);
        int per = CASES.size() + 1;
        int wantTotal = transport.equals("both") ? 2 * per : per;
        if (total != wantTotal) {
            System.out.printf("OBSERVATION-COUNT MISMATCH: ran %d checks, expected %d%n", total, wantTotal);
            System.exit(1);
        }
        System.out.printf("OBSERVATION-COUNT OK (%d expected)%n", wantTotal);
        if (failed > 0) System.exit(1);
    }

    private static void run(String label, Config cfg) {
        System.out.printf("%n=== %s ===%n", label);
        TipsyAbConfigClient cli;
        try {
            cli = TipsyAbConfigClient.create(cfg);
        } catch (Exception e) {
            System.out.printf("FAIL [%s] create: %s%n", label, e);
            failed++;
            return;
        }
        try {
            long before = cli.metrics().abtestFallbackTotal(NS);
            for (Case c : CASES) {
                String got;
                try {
                    var abctx = cli.newAbtestContext(c.uid(), null);
                    got = cli.getConfig(abctx, NS, c.key(), "<DEFAULT>");
                } catch (Exception e) {
                    System.out.printf("FAIL [%s] %s/%s: %s%n", label, c.uid(), c.key(), e);
                    failed++;
                    continue;
                }
                if (c.want().equals(got)) {
                    System.out.printf("PASS [%s] %-13s %-11s = %-12s (%s)%n", label, c.uid(), c.key(), "'" + got + "'", c.why());
                    passed++;
                } else {
                    System.out.printf("FAIL [%s] %-13s %-11s got='%s' want='%s' (%s)%n", label, c.uid(), c.key(), got, c.want(), c.why());
                    failed++;
                }
            }
            long delta = cli.metrics().abtestFallbackTotal(NS) - before;
            if (delta == 0) {
                System.out.printf("PASS [%s] abtestFallback delta = 0 (values came from the abtest main path)%n", label);
                passed++;
            } else {
                System.out.printf("FAIL [%s] abtestFallback delta = %d (values may be fallback-delivered)%n", label, delta);
                failed++;
            }
        } finally {
            try {
                cli.close();
            } catch (Exception ignored) {
                // close failures do not affect assertion outcomes
            }
        }
    }
}
