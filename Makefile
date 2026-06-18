.PHONY: proto python-proto python-build python-test python-clean tidy test

# In-tree Go modules. Hardcoded (not derived from `go work edit -json`) so the
# loop stays deterministic in CI where `jq` may not be present and so we never
# accidentally include sibling-repo `use` lines if someone adds them locally.
SDK_MODULES := ./api/gen/go ./sdk/go/tipsyauth ./sdk/go/tipsyabconfig ./sdk/go/example

# proto: regenerate Go protobuf + gRPC stubs from api/proto/tipsy/{config,abtest}/v1.
proto:
	bash scripts/gen-proto.sh

# python-proto: regenerate Python protobuf stubs for the 2 SDK-facing proto files.
python-proto:
	bash scripts/gen-proto-py.sh

# python-clean: wipe Python build / dist / egg-info / __pycache__ artifacts.
python-clean:
	rm -rf sdk/python/dist sdk/python/build sdk/python/*.egg-info
	find sdk/python -name __pycache__ -prune -exec rm -rf {} +

# python-build: rebuild the Python wheel + sdist (requires the `build` + `twine`
# tools in the active Python env; install via `pip install build twine`).
python-build: python-clean
	cd sdk/python && python -m build && twine check dist/*

# python-test: run the Python SDK test suite.
python-test:
	cd sdk/python && pytest -q

# tidy: run `go mod tidy` for every in-tree Go module. We deliberately do NOT
# run `go work sync`: this keeps each module's `go` directive isolated (so a
# downstream module bumping its toolchain doesn't drag every SDK module along).
tidy:
	@set -euo pipefail; \
	for m in $(SDK_MODULES); do \
		echo "==> tidy $$m"; \
		( cd "$$m" && go mod tidy ); \
	done

# test: per-module test loop. `tipsyabconfig` gets `-race` to match historical
# coverage of SDK client code; other modules run without race.
test:
	@set -euo pipefail; \
	for m in $(SDK_MODULES); do \
		if [ -z "$$(find "$$m" -name '*_test.go' -print -quit)" ]; then \
			echo "==> skip $$m (no _test.go)"; \
			continue; \
		fi; \
		race=""; \
		if [ "$$m" = "./sdk/go/tipsyabconfig" ]; then race="-race"; fi; \
		echo "==> test $$m $$race"; \
		( cd "$$m" && go test ./... -count=1 $$race -timeout=240s ); \
	done
