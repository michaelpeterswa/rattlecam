GO ?= go

.PHONY: all
all: hooks tools ## Wire up git hooks and install local tooling

.PHONY: hooks
hooks: ## Point git at .githooks so commit messages are linted
	git config core.hooksPath .githooks

.PHONY: tools
tools: ## Install commitlint locally (used by the commit-msg hook)
	npm install --no-save @commitlint/cli @commitlint/config-conventional

.PHONY: build
build: ## Build both binaries
	$(GO) build -o rattlecam ./cmd/rattlecam
	$(GO) build -o preview ./cmd/preview

.PHONY: test
test: ## Run the test suite
	$(GO) test -race -shuffle=on ./...

.PHONY: lint
lint: ## Run every linter CI runs
	golangci-lint run
	yamllint .
	hadolint Dockerfile

.PHONY: image
image: ## Build the container image
	docker build -t rattlecam:dev .

.PHONY: clean
clean:
	rm -f rattlecam preview
	rm -rf out

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
