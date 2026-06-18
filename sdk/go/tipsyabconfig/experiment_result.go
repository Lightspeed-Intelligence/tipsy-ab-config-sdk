package tipsyabconfig

import (
	"context"
	"errors"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	"github.com/google/uuid"
)

// ExperimentType is the SDK-stable mirror of abtestv1.ExperimentType for the
// GetExperimentResult client surface (design 04 §B.6). It lets business code
// select config_version vs custom_params experiments without importing the
// generated proto package directly.
type ExperimentType int32

const (
	// ExperimentTypeUnspecified is treated server-side as the default
	// (custom_params).
	ExperimentTypeUnspecified ExperimentType = ExperimentType(abtestv1.ExperimentType_EXPERIMENT_TYPE_UNSPECIFIED)
	// ExperimentTypeConfigVersion selects config_version experiments.
	ExperimentTypeConfigVersion ExperimentType = ExperimentType(abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION)
	// ExperimentTypeCustomParams selects custom_params experiments.
	ExperimentTypeCustomParams ExperimentType = ExperimentType(abtestv1.ExperimentType_EXPERIMENT_TYPE_CUSTOM_PARAMS)
	// ExperimentTypeAll selects both experiment types.
	ExperimentTypeAll ExperimentType = ExperimentType(abtestv1.ExperimentType_EXPERIMENT_TYPE_ALL)
)

// ResultDisplayType is the SDK-stable mirror of abtestv1.ResultDisplayType.
type ResultDisplayType int32

const (
	// ResultDisplayUnspecified is treated server-side as the default (flat_kv).
	ResultDisplayUnspecified ResultDisplayType = ResultDisplayType(abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_UNSPECIFIED)
	// ResultDisplayFlatKv requests the flattened key→version / key→value view.
	ResultDisplayFlatKv ResultDisplayType = ResultDisplayType(abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_FLAT_KV)
	// ResultDisplayEachExperimentGroup requests the per-group result list.
	ResultDisplayEachExperimentGroup ResultDisplayType = ResultDisplayType(abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP)
)

// ExperimentResultRequest is the SDK-stable request for the GetExperimentResult
// client (design 04 §B.6). It exposes every wire parameter so business code can
// fetch custom_params results (or per-group results) directly, without going
// through the config-version fast path baked into GetConfig.
type ExperimentResultRequest struct {
	// Namespace is the target namespace. Empty resolves to the project default
	// namespace (decision A-3); empty with no default returns
	// ErrNamespaceRequired; an unsubscribed ns returns
	// ErrNamespaceNotSubscribed.
	Namespace string

	// UserInfo carries the user identity (uid + attrs) sent on the wire.
	UserInfo UserInfo

	// LayerIds optionally restricts the computation to specific layers; empty
	// means all layers.
	LayerIds []string

	// Type selects the experiment type(s) to evaluate. Zero value
	// (ExperimentTypeUnspecified) is treated server-side as the default.
	Type ExperimentType

	// DisplayType selects the result shape. Zero value
	// (ResultDisplayUnspecified) is treated server-side as flat_kv.
	DisplayType ResultDisplayType

	// TraceID is the optional per-request trace id surfaced on the proto wire
	// as GetExperimentResultRequest.trace_id (design 04 §B.6, sdk-trace-id
	// §4). Empty ⇒ the SDK generates a fresh uuid.New().String() before the
	// RPC so the SDK logs and the server-side log line share the same id.
	// Non-empty ⇒ passed through verbatim (no format check). The server
	// performs an additional 128-char soft truncation; see internal/traceid.
	TraceID string
}

// GetExperimentResult is the thin exported wrapper over
// AbtestService.GetExperimentResult (design 04 §B.6). Unlike GetConfig it does
// NOT memoise into an AbtestContext and does NOT touch the local config cache —
// it returns the raw proto response so business code can read
// config_flat_kv / custom_flat_kv / groups / gray_hits directly.
//
// Namespace resolution mirrors GetConfig (explicit > default >
// ErrNamespaceRequired; unsubscribed > ErrNamespaceNotSubscribed). The call is
// bounded by Config.AbtestTimeout derived from ctx. When the abtest service was
// not configured at Init, it returns an error.
func (c *Client) GetExperimentResult(ctx context.Context, req ExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error) {
	if c == nil {
		return nil, ErrClosed
	}
	ns, err := c.resolveNamespace(req.Namespace)
	if err != nil {
		return nil, err
	}
	if c.abtestTr == nil {
		return nil, errors.New("tipsyabconfig: abtest service not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.AbtestTimeout)
	defer cancel()
	// trace_id: empty ⇒ generate locally so the SDK log and the server log
	// share the same id (server-side normalization would otherwise generate a
	// fresh one we'd never see in this process's logs).
	traceID := req.TraceID
	if traceID == "" {
		traceID = uuid.New().String()
	}
	pbReq := &abtestv1.GetExperimentResultRequest{
		Namespace:      ns,
		UserId:         req.UserInfo.UID,
		UserAttrs:      encodeUserAttrs(req.UserInfo.Attrs, c.logger),
		LayerIds:       req.LayerIds,
		ExperimentType: abtestv1.ExperimentType(req.Type),
		DisplayType:    abtestv1.ResultDisplayType(req.DisplayType),
		TraceId:        traceID,
	}
	return c.abtestTr.GetExperimentResult(callCtx, pbReq)
}
