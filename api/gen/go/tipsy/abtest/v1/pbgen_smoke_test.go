package abtestv1_test

import (
	"reflect"
	"testing"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

func TestServiceDescriptor(t *testing.T) {
	if abtestv1.AbtestService_ServiceDesc.ServiceName != "tipsy.abtest.v1.AbtestService" {
		t.Fatalf("ServiceName = %q", abtestv1.AbtestService_ServiceDesc.ServiceName)
	}
	if len(abtestv1.AbtestService_ServiceDesc.Methods) != 3 {
		t.Fatalf("methods = %d", len(abtestv1.AbtestService_ServiceDesc.Methods))
	}
}

func TestFullMethodNames(t *testing.T) {
	if abtestv1.AbtestService_GetExperimentResult_FullMethodName != "/tipsy.abtest.v1.AbtestService/GetExperimentResult" {
		t.Fatalf("GetExperimentResult FullMethodName = %q", abtestv1.AbtestService_GetExperimentResult_FullMethodName)
	}
	if abtestv1.AbtestService_GetPossibleVersionsAndExperimentSnapshotSeq_FullMethodName != "/tipsy.abtest.v1.AbtestService/GetPossibleVersionsAndExperimentSnapshotSeq" {
		t.Fatalf("GPV FullMethodName = %q", abtestv1.AbtestService_GetPossibleVersionsAndExperimentSnapshotSeq_FullMethodName)
	}
	if abtestv1.AbtestService_GetExperimentSnapshotSeq_FullMethodName != "/tipsy.abtest.v1.AbtestService/GetExperimentSnapshotSeq" {
		t.Fatalf("GetExperimentSnapshotSeq FullMethodName = %q", abtestv1.AbtestService_GetExperimentSnapshotSeq_FullMethodName)
	}
}

func TestServerInterfaceHasMethods(t *testing.T) {
	var _ abtestv1.AbtestServiceServer = abtestv1.UnimplementedAbtestServiceServer{}
	srvType := reflect.TypeOf((*abtestv1.AbtestServiceServer)(nil)).Elem()
	needs := []string{"GetExperimentResult", "GetPossibleVersionsAndExperimentSnapshotSeq", "GetExperimentSnapshotSeq"}
	for _, name := range needs {
		if _, ok := srvType.MethodByName(name); !ok {
			t.Fatalf("AbtestServiceServer missing method %s", name)
		}
	}
}

func TestNilReceiverGetters(t *testing.T) {
	var r *abtestv1.GetExperimentResultRequest
	if r.GetNamespace() != "" || r.GetUserId() != "" || r.GetUserAttrs() != nil {
		t.Fatalf("nil GetExperimentResultRequest getters non-default")
	}
	var resp *abtestv1.GetExperimentResultResponse
	if resp.GetConfigFlatKv() != nil || resp.GetExposures() != nil || resp.GetComputedAt() != nil {
		t.Fatalf("nil GetExperimentResultResponse getters non-default")
	}
	var gpv *abtestv1.GetPossibleVersionsAndExperimentSnapshotSeqResponse
	if gpv.GetPossibleVersions() != nil || gpv.GetExperimentSnapshotSeq() != 0 {
		t.Fatalf("nil GPV resp getters non-default")
	}
	var vl *abtestv1.VersionList
	if vl.GetVersionIds() != nil {
		t.Fatalf("nil VersionList getter non-default")
	}
	var v *abtestv1.Value
	if v.GetV() != nil {
		t.Fatalf("nil Value getter non-default")
	}
	var exp *abtestv1.Exposure
	if exp.GetKey() != "" || exp.GetSource() != "" || exp.GetVersion() != 0 ||
		exp.GetExperimentId() != "" || exp.GetGroupId() != "" || exp.GetReleaseId() != 0 ||
		exp.GetExperimentStatus() != "" {
		t.Fatalf("nil Exposure getter non-default")
	}
}

func TestValueOneofTypes(t *testing.T) {
	v := &abtestv1.Value{V: &abtestv1.Value_S{S: "hello"}}
	if v.GetV() == nil {
		t.Fatal("V nil")
	}
	if _, ok := v.GetV().(*abtestv1.Value_S); !ok {
		t.Fatalf("Value_S not matched")
	}

	v = &abtestv1.Value{V: &abtestv1.Value_I{I: 42}}
	if _, ok := v.GetV().(*abtestv1.Value_I); !ok {
		t.Fatalf("Value_I")
	}
	v = &abtestv1.Value{V: &abtestv1.Value_D{D: 3.14}}
	if _, ok := v.GetV().(*abtestv1.Value_D); !ok {
		t.Fatalf("Value_D")
	}
	v = &abtestv1.Value{V: &abtestv1.Value_B{B: true}}
	if _, ok := v.GetV().(*abtestv1.Value_B); !ok {
		t.Fatalf("Value_B")
	}
}
