package io.github.lightspeedintelligence.abconfig;

import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.netty.shaded.io.grpc.netty.GrpcSslContexts;
import io.grpc.netty.shaded.io.grpc.netty.NettyChannelBuilder;
import io.grpc.netty.shaded.io.netty.handler.ssl.SslContext;
import io.grpc.netty.shaded.io.netty.handler.ssl.util.InsecureTrustManagerFactory;
import java.util.concurrent.TimeUnit;
import java.util.function.UnaryOperator;
import javax.net.ssl.SSLException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Builds gRPC {@link ManagedChannel}s from a parsed {@link GrpcTarget}.
 *
 * <p>Port of the Go {@code (*Client).dial} ({@code sdk.go}). The target carries
 * the transport decision (plaintext h2c vs TLS) and the TLS details (authority /
 * SNI / skip-verify) produced by {@link GrpcTarget#parseGrpcTarget(String)}.
 *
 * <p>Builder selection (per design 04): plaintext uses
 * {@code ManagedChannelBuilder.forTarget(target).usePlaintext()}; TLS (including
 * skip-verify) uses the <strong>shaded</strong>
 * {@link NettyChannelBuilder}{@code .forTarget(target).sslContext(sslContext)}
 * with a {@link SslContext} from the shaded {@link GrpcSslContexts}. The two
 * builder types are both {@link ManagedChannelBuilder} subtypes, so the common
 * defaults and the {@code channelConfigurator} injection seam operate on a
 * {@code ManagedChannelBuilder<?>} reference.
 *
 * <p>Per-RPC credentials are <em>not</em> attached at the channel level: the
 * bearer token travels as per-stub {@link io.grpc.CallCredentials} (see
 * {@link TokenSource#toCallCredentials()}), wired by ST3 when it assembles the
 * stubs. Likewise the outbound message size is set per-stub via
 * {@code withMaxOutboundMessageSize} in ST3.
 */
final class GrpcChannels {

    private static final Logger LOG = LoggerFactory.getLogger(GrpcChannels.class);

    private GrpcChannels() {
    }

    /**
     * Builds a {@link ManagedChannel} for the given parsed target.
     *
     * <p>Order of operations:
     * <ol>
     *   <li>Construct the transport-specific builder (plaintext {@code usePlaintext}
     *       vs shaded Netty {@code sslContext}); for {@code insecureSkipVerify}
     *       trust all certificates and {@code WARN} once.</li>
     *   <li>Apply common defaults via a {@code ManagedChannelBuilder<?>} reference:
     *       max inbound message size, keepalive (30s/5s/without-calls), authority
     *       override, and {@code round_robin} for {@code dns:///} targets.</li>
     *   <li>If {@code channelConfigurator} is non-null, invoke it once on the
     *       builder (the injection seam — tests can swap in an in-process channel
     *       or add production overrides; equivalent to Go appending
     *       {@code cfg.DialOptions} last).</li>
     *   <li>{@code build()} the channel.</li>
     * </ol>
     *
     * @param tgt                 the parsed target
     * @param maxRecvBytes        the channel-level max inbound message size
     * @param channelConfigurator optional final builder customisation; may be
     *                            {@code null}
     * @return the built (lazily-connecting) channel
     */
    static ManagedChannel dial(GrpcTarget tgt, int maxRecvBytes,
            UnaryOperator<ManagedChannelBuilder<?>> channelConfigurator) {
        ManagedChannelBuilder<?> builder;
        if (tgt.useTls()) {
            NettyChannelBuilder nettyBuilder =
                    NettyChannelBuilder.forTarget(tgt.dialTarget()).sslContext(buildSslContext(tgt));
            builder = nettyBuilder;
        } else {
            builder = ManagedChannelBuilder.forTarget(tgt.dialTarget()).usePlaintext();
        }

        // Common defaults (all available on ManagedChannelBuilder).
        builder.maxInboundMessageSize(maxRecvBytes)
                .keepAliveTime(30, TimeUnit.SECONDS)
                .keepAliveTimeout(5, TimeUnit.SECONDS)
                .keepAliveWithoutCalls(true);

        if (!tgt.authority().isEmpty()) {
            // Drives the HTTP/2 :authority and, under TLS, the SNI / ServerName
            // (set on the SslContext side via the trust config; overrideAuthority
            // pins the certificate-name target for verification).
            builder.overrideAuthority(tgt.authority());
        }

        // Headless Service + client-side round_robin opt-in for dns:/// targets;
        // every other address keeps the default pick_first.
        String lbPolicy = GrpcTarget.loadBalancingPolicyFor(tgt.dialTarget());
        if (lbPolicy != null) {
            builder.defaultLoadBalancingPolicy(lbPolicy);
        }

        // Injection seam: invoked once after the SDK's own defaults.
        if (channelConfigurator != null) {
            builder = channelConfigurator.apply(builder);
        }

        return builder.build();
    }

    /**
     * Builds the shaded {@link SslContext} for a TLS target. Plain TLS uses
     * {@code GrpcSslContexts.forClient().build()}; {@code insecureSkipVerify}
     * installs the shaded {@link InsecureTrustManagerFactory} (trust-all) and
     * logs a {@code WARN} once per dial.
     */
    private static SslContext buildSslContext(GrpcTarget tgt) {
        try {
            if (tgt.insecureSkipVerify()) {
                LOG.warn("tipsyabconfig: TLS certificate verification DISABLED (insecure=true); "
                        + "Dev / Origin-Cert direct-IP only — never use in production (authority={})",
                        tgt.authority());
                return GrpcSslContexts.forClient()
                        .trustManager(InsecureTrustManagerFactory.INSTANCE)
                        .build();
            }
            return GrpcSslContexts.forClient().build();
        } catch (SSLException e) {
            throw new TipsyConfigException(
                    "tipsyabconfig: failed to build TLS context for " + tgt.dialTarget(), e);
        }
    }
}
