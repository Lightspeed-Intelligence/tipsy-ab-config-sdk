package tipsyabconfig

import (
	"context"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// Transport mode identifiers for Config.Transport / the TIPSY_SDK_TRANSPORT
// environment variable. An empty value selects the default (gRPC).
const (
	// TransportGRPC selects gRPC transport (the default). Addresses are gRPC
	// targets; Subscribe streaming is available.
	TransportGRPC = "grpc"
	// TransportHTTP selects HTTP transport. ConfigServiceAddr / AbtestServiceAddr
	// are interpreted as base URLs (http:// or https://). Subscribe is NOT
	// available — the SDK relies solely on periodic PullAll polling.
	TransportHTTP = "http"
)

// transportEnvVarName is the environment variable the SDK reads at Init to
// select the transport mode when Config.Transport is empty.
//
// Note: the identifier intentionally avoids the bare `transportEnvVar` name,
// which the package test file pins independently as a contract guard.
const transportEnvVarName = "TIPSY_SDK_TRANSPORT"

// configTransport is the SDK-internal abstraction over the ConfigService calls
// the SDK actually issues. It is satisfied by both the gRPC wrapper
// (grpcConfigTransport) and the HTTP implementation (httpConfigTransport).
//
// Subscribe is intentionally NOT part of this interface: it is a gRPC-only
// streaming path. runSubscribe is started only in gRPC mode and uses the gRPC
// client directly.
type configTransport interface {
	PullAll(ctx context.Context, req *configv1.PullAllRequest) (*configv1.PullAllResponse, error)
}

// abtestTransport is the SDK-internal abstraction over the AbtestService calls
// the SDK actually issues. It is satisfied by both the gRPC wrapper
// (grpcAbtestTransport) and the HTTP implementation (httpAbtestTransport).
type abtestTransport interface {
	GetExperimentResult(ctx context.Context, req *abtestv1.GetExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error)
}

// grpcConfigTransport is a thin, zero-logic wrapper over the generated
// ConfigService gRPC client. It forwards calls byte-for-byte so the gRPC code
// path behaves exactly as before the transport abstraction was introduced.
type grpcConfigTransport struct {
	cli configv1.ConfigServiceClient
}

func (t grpcConfigTransport) PullAll(ctx context.Context, req *configv1.PullAllRequest) (*configv1.PullAllResponse, error) {
	return t.cli.PullAll(ctx, req)
}

// grpcAbtestTransport is a thin, zero-logic wrapper over the generated
// AbtestService gRPC client.
type grpcAbtestTransport struct {
	cli abtestv1.AbtestServiceClient
}

func (t grpcAbtestTransport) GetExperimentResult(ctx context.Context, req *abtestv1.GetExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error) {
	return t.cli.GetExperimentResult(ctx, req)
}

// newGRPCConfigTransport wraps a ConfigService gRPC client as a configTransport.
func newGRPCConfigTransport(cli configv1.ConfigServiceClient) configTransport {
	return grpcConfigTransport{cli: cli}
}

// newGRPCAbtestTransport wraps an AbtestService gRPC client as an
// abtestTransport.
func newGRPCAbtestTransport(cli abtestv1.AbtestServiceClient) abtestTransport {
	return grpcAbtestTransport{cli: cli}
}
