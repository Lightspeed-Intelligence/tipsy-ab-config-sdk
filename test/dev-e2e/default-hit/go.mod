module github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/test/dev-e2e/default-hit

go 1.25.0

// Workspace-only driver: go.work resolves tipsyabconfig to the in-tree copy
// under test. The pin is the last released version; GOWORK=off against it
// fails the run loudly (no default line before the version that adds it).
require github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig v0.15.1
