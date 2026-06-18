module github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig

go 1.25.0

require (
	github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go v0.0.0
	github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth v0.0.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
)

require (
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260420184626-e10c466a9529 // indirect
)

// Same-repo replace directives. go.work resolves these locally during
// development too, but the bare `v0.0.0` require above is not a valid
// pseudo-version, so without these replace lines `go build` / `go test`
// from within this module fail to resolve the sibling modules. When real
// tags are pushed (sdk/go/tipsyauth/vX.Y.Z, api/gen/go/vX.Y.Z), bump the
// require versions above and these replace lines can be dropped (or kept
// behind a build-time `sed` like ab-config's Dockerfile does).
replace github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go => ../../../api/gen/go

replace github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth => ../tipsyauth
