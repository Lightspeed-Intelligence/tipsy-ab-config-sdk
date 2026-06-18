package configv1_test

import (
	"reflect"
	"testing"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

func TestPbSmoke_ServiceDescriptor(t *testing.T) {
	if configv1.ConfigService_ServiceDesc.ServiceName != "tipsy.config.v1.ConfigService" {
		t.Errorf("ServiceName = %q", configv1.ConfigService_ServiceDesc.ServiceName)
	}
}

func TestPbSmoke_ServerInterface(t *testing.T) {
	// Interface must require all SubTask 2 methods.
	srv := reflect.TypeOf((*configv1.ConfigServiceServer)(nil)).Elem()
	want := []string{
		"PullAll",
		"Subscribe",
		"ListNamespacesByKind",
		"ListConfigKeys",
		"ListConfigVersions",
		"NotifyBusinessNamespaceChange",
	}
	for _, name := range want {
		if _, ok := srv.MethodByName(name); !ok {
			t.Errorf("ConfigServiceServer missing %s", name)
		}
	}
}

func TestPbSmoke_FullMethodNames(t *testing.T) {
	pairs := map[string]string{
		configv1.ConfigService_PullAll_FullMethodName:                       "/tipsy.config.v1.ConfigService/PullAll",
		configv1.ConfigService_Subscribe_FullMethodName:                     "/tipsy.config.v1.ConfigService/Subscribe",
		configv1.ConfigService_ListNamespacesByKind_FullMethodName:          "/tipsy.config.v1.ConfigService/ListNamespacesByKind",
		configv1.ConfigService_ListConfigKeys_FullMethodName:                "/tipsy.config.v1.ConfigService/ListConfigKeys",
		configv1.ConfigService_ListConfigVersions_FullMethodName:            "/tipsy.config.v1.ConfigService/ListConfigVersions",
		configv1.ConfigService_NotifyBusinessNamespaceChange_FullMethodName: "/tipsy.config.v1.ConfigService/NotifyBusinessNamespaceChange",
	}
	for got, want := range pairs {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestPbSmoke_NamespaceKindEnum(t *testing.T) {
	if configv1.NamespaceKind_NAMESPACE_KIND_UNSPECIFIED != 0 ||
		configv1.NamespaceKind_NAMESPACE_KIND_BUSINESS != 1 {
		t.Error("enum values changed")
	}
}

func TestPbSmoke_MessageGettersNilSafe(t *testing.T) {
	var (
		nilReq *configv1.PullAllRequest
		nilSub *configv1.SubscribeRequest
		nilSnp *configv1.NamespaceSnapshot
		nilKS  *configv1.KeyState
	)
	if nilReq.GetNamespaces() != nil {
		t.Error("nil PullAllRequest.GetNamespaces")
	}
	if nilSub.GetNamespaces() != nil || nilSub.GetKnownSeqs() != nil {
		t.Error("nil SubscribeRequest getters")
	}
	if nilSnp.GetNamespace() != "" || nilSnp.GetBusinessSnapshotSeq() != 0 {
		t.Error("nil NamespaceSnapshot getters")
	}
	if nilKS.GetKey() != "" || nilKS.GetFullReleaseVersion() != 0 {
		t.Error("nil KeyState getters")
	}
}
