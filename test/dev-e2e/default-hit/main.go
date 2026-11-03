// Command default-hit is the SDK dev-e2e for the default-value hit log
// (platform task real-enrollment-dashboard, design part-c §C6 "SDK 仓的
// dev-e2e").
//
// Scenario: on a REAL local platform, a user who lands in an experiment's
// empty-parameter group reads a key that the experiment controls (another
// group configures it) and that has NO full release. The SDK must:
//   - return the caller's default;
//   - log exactly one `tipsyabconfig: get_config hit (default)` line with
//     `reason=default default_cause=no_full_release` on stdout.
//
// A control user in the configured group must get the configured value with a
// `reason=experiment` hit line. That proves the key really is controlled
// (has_dynamic_resolution=true) and the experiment is live, so the default line
// is not explained by fast_path / abtest_unavailable / unknown_key.
//
// Group membership is read from the live server (public GetExperimentResult,
// EACH_EXPERIMENT_GROUP), never predicted locally, so this does not re-derive
// the bucketing it depends on. The topology is created by run.sh through the
// platform admin API.
//
// The SDK's slog text output goes to stdout (run.sh greps it) and to an
// in-memory copy this driver asserts on. Exit status 0 means every assertion
// held.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

const (
	defaultValue    = "DEF-default-hit-e2e"
	traceEmpty      = "default-hit-e2e-empty"
	traceConfigured = "default-hit-e2e-configured"
	maxCandidates   = 2000
)

// syncBuffer is a goroutine-safe copy of everything the SDK logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(b.buf.String(), "\n")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "DEFAULT-HIT E2E FAIL: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	grpcAddr := flag.String("grpc", "localhost:25252", "platform gRPC address (plaintext host:port)")
	ns := flag.String("ns", "", "business namespace created by run.sh")
	key := flag.String("key", "", "config key controlled by the experiment, with NO full release")
	expID := flag.String("experiment", "", "running config_version experiment id")
	emptyGroup := flag.String("empty-group", "", "group id whose params are empty")
	cfgGroup := flag.String("configured-group", "", "group id that configures -key")
	cfgValue := flag.String("configured-value", "", "value of the version the configured group serves")
	waitFor := flag.Duration("wait", 30*time.Second, "how long to wait for the experiment to appear in results")
	flag.Parse()
	token := os.Getenv("AB_CONFIG_TOKEN")
	for name, v := range map[string]string{"-ns": *ns, "-key": *key, "-experiment": *expID, "-empty-group": *emptyGroup,
		"-configured-group": *cfgGroup, "-configured-value": *cfgValue, "AB_CONFIG_TOKEN": token} {
		if v == "" {
			fail("%s is required", name)
		}
	}

	ctx := context.Background()
	captured := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, captured), &slog.HandlerOptions{Level: slog.LevelInfo}))
	cli, err := tipsyabconfig.Init(ctx, tipsyabconfig.Config{
		Namespaces:        []string{*ns},
		ConfigServiceAddr: *grpcAddr,
		AbtestServiceAddr: *grpcAddr,
		Token:             token,
		Logger:            logger,
	})
	if err != nil {
		fail("Init: %v", err)
	}
	defer cli.Close()

	emptyUID, cfgUID := findUsers(ctx, cli, *ns, *expID, *emptyGroup, *cfgGroup, *waitFor)
	fmt.Printf("default-hit e2e: empty-param-group uid=%s configured-group uid=%s\n", emptyUID, cfgUID)

	// Control: the configured group really steers the key.
	v, err := cli.GetConfig(ctx, cli.NewAbtestContextWithTraceID(ctx, cfgUID, nil, traceConfigured), *ns, *key, defaultValue)
	if err != nil || v != *cfgValue {
		fail("configured-group control GetConfig = (%q, %v), want (%q, nil)", v, err, *cfgValue)
	}
	// Scenario under test: empty-parameter group, key without a full release.
	v, err = cli.GetConfig(ctx, cli.NewAbtestContextWithTraceID(ctx, emptyUID, nil, traceEmpty), *ns, *key, defaultValue)
	if err != nil || v != defaultValue {
		fail("empty-param-group GetConfig = (%q, %v), want the caller's default (%q, nil)", v, err, defaultValue)
	}

	cfgLines := linesWith(captured.lines(), "trace_id="+traceConfigured, "get_config hit")
	if len(cfgLines) != 1 || !strings.Contains(cfgLines[0], `msg="tipsyabconfig: get_config hit (abtest)"`) ||
		!strings.Contains(cfgLines[0], " reason=experiment ") || !strings.Contains(cfgLines[0], " group_id="+*cfgGroup+" ") {
		fail("configured-group control: want exactly one (abtest) reason=experiment line for group %s, got %q", *cfgGroup, cfgLines)
	}
	defLines := linesWith(captured.lines(), "trace_id="+traceEmpty, "get_config hit")
	if len(defLines) != 1 {
		fail("empty-param-group: want exactly one get_config hit line, got %d: %q", len(defLines), defLines)
	}
	line := defLines[0]
	for _, want := range []string{
		`msg="tipsyabconfig: get_config hit (default)"`,
		" ns=" + *ns + " ",
		" key=" + *key + " ",
		" reason=default default_cause=no_full_release ",
		" uid=" + emptyUID + " ",
	} {
		if !strings.Contains(line, want) {
			fail("default line lacks %q: %s", want, line)
		}
	}
	for _, banned := range []string{" version=", " experiment_id=", " group_id=", " release_id=", defaultValue} {
		if strings.Contains(line, banned) {
			fail("default line must not carry %q: %s", banned, line)
		}
	}
	fmt.Println("DEFAULT-HIT E2E PASS: empty-param-group user logged reason=default default_cause=no_full_release")
}

// findUsers scans deterministic candidate uids and asks the LIVE server which
// group of expID each lands in, returning one uid per target group. The
// empty-parameter group must come back with empty params_versions on the wire,
// which is the shape §C1's no_full_release row is about.
func findUsers(ctx context.Context, cli *tipsyabconfig.Client, ns, expID, emptyGroup, cfgGroup string, wait time.Duration) (string, string) {
	deadline := time.Now().Add(wait)
	for {
		var emptyUID, cfgUID string
		seen := false
		for i := 1; i <= maxCandidates && (emptyUID == "" || cfgUID == ""); i++ {
			uid := fmt.Sprintf("default-hit-user-%d", i)
			resp, err := cli.GetExperimentResult(ctx, tipsyabconfig.ExperimentResultRequest{
				Namespace:   ns,
				UserInfo:    tipsyabconfig.UserInfo{ExperimentHashID: uid},
				Type:        tipsyabconfig.ExperimentTypeConfigVersion,
				DisplayType: tipsyabconfig.ResultDisplayEachExperimentGroup,
			})
			if err != nil {
				fail("GetExperimentResult(%s): %v", uid, err)
			}
			for _, g := range resp.GetGroups() {
				if g.GetExperimentId() != expID {
					continue
				}
				seen = true
				switch g.GetGroupId() {
				case emptyGroup:
					if n := len(g.GetParamsVersions()); n != 0 {
						fail("empty-param group %s came back with %d params", emptyGroup, n)
					}
					if emptyUID == "" {
						emptyUID = uid
					}
				case cfgGroup:
					if cfgUID == "" {
						cfgUID = uid
					}
				}
			}
			if !seen && i >= 20 {
				break // experiment not visible yet; re-poll after the cache refresh
			}
		}
		if emptyUID != "" && cfgUID != "" {
			return emptyUID, cfgUID
		}
		if time.Now().After(deadline) {
			fail("after %s: experiment %s visible=%v, empty-group uid=%q, configured-group uid=%q", wait, expID, seen, emptyUID, cfgUID)
		}
		time.Sleep(time.Second)
	}
}

func linesWith(lines []string, subs ...string) []string {
	var out []string
	for _, l := range lines {
		ok := true
		for _, s := range subs {
			if !strings.Contains(l, s) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, l)
		}
	}
	return out
}
