.DEFAULT_GOAL := help
SHELL := /bin/bash

IMAGE   ?= docverify:local
CLUSTER ?= docverify
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Sync go.mod / go.sum
	go mod tidy

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./internal/...

.PHONY: cover
cover: ## Report total test coverage
	go test -count=1 -coverprofile=coverage.out -coverpkg=./internal/...,./cmd/... ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Open the HTML coverage report
	go tool cover -html=coverage.out -o coverage.html
	@echo "Wrote coverage.html"

.PHONY: integration
integration: ## Run the Ginkgo integration suite
	go test -race -count=1 -v ./test/integration/

.PHONY: bench
bench: ## Run benchmarks with allocation stats
	go test -bench=. -benchmem -run='^$$' ./internal/store/

.PHONY: proto
proto: ## Regenerate gRPC stubs from the .proto file
	protoc --go_out=. --go_opt=module=github.com/sparrow000iv/go-docverify-service \
	       --go-grpc_out=. --go-grpc_opt=module=github.com/sparrow000iv/go-docverify-service \
	       proto/docverify/v1/docverify.proto

.PHONY: build
build: ## Compile the server binary
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/server ./cmd/server

.PHONY: run
run: ## Run the server locally
	go run ./cmd/server

.PHONY: docker-build
docker-build: ## Build the container image
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) .

.PHONY: kind-up
kind-up: ## Create the local kind cluster
	kind create cluster --config deploy/kind-cluster.yaml

.PHONY: kind-load
kind-load: docker-build ## Load the image into kind
	kind load docker-image $(IMAGE) --name $(CLUSTER)

.PHONY: deploy
deploy: kind-load ## Deploy to the kind cluster
	kubectl apply -f deploy/k8s/
	kubectl rollout status deployment/docverify --timeout=120s

.PHONY: smoke
smoke: ## Run the end-to-end smoke test against the cluster
	./scripts/smoke_test.sh

.PHONY: logs
logs: ## Tail service logs
	kubectl logs -l app=docverify -f --tail=50

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(CLUSTER)

.PHONY: ci
ci: fmt vet test integration cover ## Run the full local CI pipeline

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out coverage.html
