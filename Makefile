# Common tasks for mobd. Run `make` (or `make help`) to list them.

GO      ?= go
BIN     ?= bin/mobd
ARGS    ?=

.DEFAULT_GOAL := help
.PHONY: help build test race vet fmt check clean \
	init serve start stop restart status logs release-test

help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- mobd (office Mac) ---

build: ## Build mobd into bin/mobd
	$(GO) build -o $(BIN) ./cmd/mobd

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

release-test: ## Test R2 publishing and installer
	node --test .github/release/*.test.mjs

check: vet test release-test ## Go vet, Go tests and distribution tests

clean: ## Remove build outputs
	rm -rf bin dist
