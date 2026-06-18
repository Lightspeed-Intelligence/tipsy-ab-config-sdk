package tipsyauth

import (
	"go/build"
	"strings"
	"testing"
)

// TestTipsyAuthDoesNotImportInternal enforces design 04 acceptance:
// "tipsyauth 不 import 任何 internal/...". The public package must remain
// importable by external modules (e.g. tipsy-backend), so it MUST NOT pull in
// any github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/internal/... package — directly. We scan the
// package's own (non-test) import list via go/build.
//
// This guards the import-direction invariant: internal/auth may import
// tipsyauth, but never the reverse.
func TestTipsyAuthDoesNotImportInternal(t *testing.T) {
	pkg, err := build.ImportDir(".", build.ImportComment)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, "/internal/") || strings.HasSuffix(imp, "/internal") {
			t.Errorf("tipsyauth imports internal package %q; the public signer must not depend on internal/...", imp)
		}
		if strings.HasPrefix(imp, "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/internal") {
			t.Errorf("tipsyauth imports %q; must not depend on internal/...", imp)
		}
	}
}
