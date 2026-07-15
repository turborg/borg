.DEFAULT_GOAL := help
SHELL := /usr/bin/env bash

# Build flags mirror .goreleaser.yml so `make build` and a release
# binary differ only by version stamp.
VERSION ?= dev
LDFLAGS := -s -w -X github.com/turborg/borg/internal/version.Version=$(VERSION)
GO_BUILD_FLAGS := -trimpath -ldflags="$(LDFLAGS)"

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS=":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the CLI into ./bin/borg (+ a ./bin/turborg alias — same binary, both names work).
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/borg ./cmd/borg
	@ln -sf borg bin/turborg

.PHONY: test
test: ## Run the full test suite with race detector.
	go test -race -count=1 -timeout 120s ./...

.PHONY: cover
cover: ## Run tests + write coverage profile + print summary.
	go test -race -count=1 -coverprofile=coverage.out ./internal/...
	@go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint against the whole tree.
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format every Go file in place.
	gofmt -s -w .

.PHONY: vet
vet: ## go vet on every package.
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod + go.sum.
	go mod tidy

.PHONY: docker
docker: ## Build the borg image as borg:dev (the Go build runs inside Docker).
	docker build -t borg:dev --build-arg VERSION=$(VERSION) .

.PHONY: docker-test
docker-test: ## Run the test suite inside a container so dependency code never executes on the host.
	docker run --rm \
		-v "$(CURDIR)":/src:ro \
		-v borg-go-mod:/go/pkg/mod \
		-w /src \
		golang:1.26 \
		sh -c 'go test -race -count=1 -timeout 120s ./...'

.PHONY: cover-gate
cover-gate: ## Run tests in Docker + enforce the >=90% total coverage gate.
	docker run --rm \
		-v "$(CURDIR)":/src:ro \
		-v borg-go-mod:/go/pkg/mod \
		-w /src \
		golang:1.26 \
		sh -c 'go test -count=1 -coverprofile=/tmp/cov.out ./internal/... && \
			go run github.com/vladopajic/go-test-coverage/v2@latest --config .testcoverage.yml --profile /tmp/cov.out'

.PHONY: eval
eval: ## Agent eval suite (internal/eval) in Docker. Deterministic cassette/oracle evals always run; the live eval (floko + chuppa, never axiom) runs only with BORG_EVAL=1 + borg auth (mounts ~/.config/borg) and costs tokens. Override models with BORG_EVAL_MODELS=floko,chuppa.
	docker run --rm \
		-v "$(CURDIR)":/src:ro \
		-v borg-go-mod:/go/pkg/mod \
		-v "$(HOME)/.config/borg":/root/.config/borg:ro \
		-e BORG_EVAL -e BORG_EVAL_MODELS -e BORG_EVAL_EFFORT -e BORG_EVAL_MAX_STEPS \
		-e BORG_EVAL_CONCURRENCY -e BORG_EVAL_TASKS -e BORG_EVAL_SAVE_BASELINE -e BORG_EVAL_REPORT \
		-e BORG_EVAL_REPEAT -e BORG_EVAL_ALLOW_AXIOM \
		-e BORG_API_BASE_URL -e BORG_APP_URL -e BORG_LLM_PROXY_URL \
		-w /src \
		golang:1.26 \
		sh -c 'go test -count=1 -timeout 60m -v ./internal/eval/...'

.PHONY: docker-bin
docker-bin: docker ## Build in Docker and extract the binary to ./bin/borg (no host go build).
	@mkdir -p bin
	@cid=$$(docker create borg:dev); docker cp $$cid:/borg bin/borg; docker rm $$cid >/dev/null
	@ln -sf borg bin/turborg
	@echo "extracted bin/borg (+ turborg alias, built inside Docker)"

.PHONY: clean
clean: ## Remove build + coverage artifacts.
	rm -rf bin coverage.out coverage.html
