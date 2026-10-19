# Common tasks for mobd. Run `make` (or `make help`) to list them.

GO      ?= go
BIN     ?= bin/mobd
ARGS    ?=
SIGN_IDENTITY ?=

.DEFAULT_GOAL := help
.PHONY: help build test race vet fmt check clean \
	init serve start stop restart status logs release release-test

help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- mobd (office Mac) ---

build: ## Build mobd into bin/mobd (SIGN_IDENTITY=... signs it so macOS grants survive rebuilds)
	$(GO) build -o $(BIN) ./cmd/mobd
	@if [ -n "$(SIGN_IDENTITY)" ]; then tools/devsign.sh "$(SIGN_IDENTITY)" $(BIN); fi

test: ## Run Go tests
	$(GO) test ./...

race: ## Run Go tests with the race detector
	$(GO) test -race ./...

vet: ## go vet
	$(GO) vet ./...

fmt: ## gofmt all Go files
	gofmt -w cmd internal

init: build ## Create the user config (never overwrite)
	$(BIN) init $(ARGS)

serve: build ## Run mobd in the foreground
	$(BIN) serve $(ARGS)

start: build ## Start mobd via the user LaunchAgent
	$(BIN) start $(ARGS)

stop: build ## Stop mobd and disable login startup
	$(BIN) stop

restart: build ## Reload config and restart mobd
	$(BIN) restart $(ARGS)

status: build ## Show mobd status and address
	$(BIN) status

logs: build ## Follow the mobd log
	$(BIN) logs

release: export RELEASE_TAG = $(VERSION)
release: ## Create and push a release tag (VERSION=vX.Y.Z)
	node .github/release/tag.mjs "$$RELEASE_TAG"

release-test: ## Test release tagging, R2 publishing and installer
	node --test .github/release/*.test.mjs

check: vet test release-test ## Go vet, Go tests and distribution tests

clean: ## Remove build outputs
	rm -rf bin dist
