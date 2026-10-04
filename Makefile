GO ?= go
BINARY ?= nano-harness
GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.7.0
GORELEASER_VERSION ?= v2.17.1
LEFTHOOK_VERSION ?= v2.1.11
DELVE_VERSION ?= v1.27.1

GO_FILES := $(shell find cmd internal -type f -name '*.go' 2>/dev/null)

.DEFAULT_GOAL := help

.PHONY: help bootstrap hooks fmt fmt-check tidy mod-check vet lint test coverage architecture submodule agent-notes skills change-scope workflow-tools build tui-e2e tui-fixture debug-tools debug-fixture vuln release-check quick check ci clean

help: ## Show available commands.
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap: ## Install pinned contributor tools into GOBIN.
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	$(GO) install github.com/evilmartians/lefthook/v2@$(LEFTHOOK_VERSION)

hooks: ## Install the repository Git hooks with lefthook.
	lefthook install

fmt: ## Format Go source files.
	gofmt -w $(GO_FILES)

fmt-check: ## Verify Go formatting without writing files.
	@test -z "$$(gofmt -l $(GO_FILES))" || { gofmt -l $(GO_FILES); exit 1; }

tidy: ## Update go.mod and go.sum.
	$(GO) mod tidy

mod-check: ## Verify go.mod/go.sum are tidy without writing files.
	$(GO) mod tidy -diff

vet: ## Run the Go vet analyzers.
	$(GO) vet ./...

lint: ## Run the pinned golangci-lint policy (requires make bootstrap).
	golangci-lint run ./...

test: ## Run unit and package tests with the race detector.
	$(GO) test -race -count=1 ./...

coverage: ## Enforce 100% coverage for every product source file.
	scripts/coverage.sh

architecture: ## Enforce directional package dependencies.
	$(GO) run ./internal/tools/archcheck

submodule: ## Verify the reference submodule identity and cleanliness.
	scripts/verify-submodule.sh

agent-notes: ## Validate Agent Note format and, in CI, change coverage.
	scripts/verify-agent-notes.sh

skills: ## Validate repository-local skill contracts and links.
	$(GO) run ./internal/tools/skillcheck

change-scope: ## Show committed and worktree changes (set BASE_REF after the first commit).
	@if [ -n "$(BASE_REF)" ]; then scripts/change-scope.sh "$(BASE_REF)" "$(or $(HEAD_REF),HEAD)"; else scripts/change-scope.sh; fi

workflow-tools: ## Test deterministic repository workflow helpers.
	scripts/change-scope_test.sh
	scripts/coverage_test.sh
	scripts/verify-release_test.sh

build: ## Build the command from its real entry path.
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/$(BINARY) ./cmd/nano-harness
	./bin/$(BINARY) version

tui-e2e: build ## Verify real terminal/tool/subagent behavior with a local model fixture (Python 3, Unix).
	python3 scripts/tui-e2e.py --binary bin/$(BINARY)

tui-fixture: ## Serve a local Responses fixture for interactive debugging (Python 3).
	python3 scripts/tui-e2e.py --serve .cache/tui-fixture

debug-tools: ## Install the pinned Delve debugger into the ignored project cache.
	GOBIN="$(CURDIR)/.cache/debug-tools" $(GO) install github.com/go-delve/delve/cmd/dlv@$(DELVE_VERSION)

debug-fixture: ## Run the fixture TUI in this terminal and expose Delve to GoLand on loopback port 2345.
	mkdir -p bin
	$(GO) build -gcflags='all=-N -l' -o bin/nano-harness-debug ./cmd/nano-harness
	NANO_FIXTURE_KEY=fixture-key .cache/debug-tools/dlv exec bin/nano-harness-debug --headless --listen=127.0.0.1:2345 --api-version=2 --accept-multiclient -- tui --root .cache/tui-fixture/workspace --settings .cache/tui-fixture/settings.yaml --credentials .cache/tui-fixture/credentials.yaml --session-root .cache/tui-fixture/sessions

vuln: ## Check reachable dependencies against the Go vulnerability database.
	govulncheck ./...

release-check: ## Validate the GoReleaser configuration.
	goreleaser check

quick: fmt-check mod-check vet test architecture submodule agent-notes skills workflow-tools ## Run dependency-free local gates.

check: quick lint coverage build ## Run all normal pre-push gates.

ci: check vuln release-check ## Run the complete CI-equivalent gate set.

clean: ## Remove generated build and coverage output.
	rm -rf -- bin dist release-artifacts coverage.out
