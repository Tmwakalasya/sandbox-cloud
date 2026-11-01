export GOTOOLCHAIN := local
export GOPROXY := off
export GOSUMDB := off

.PHONY: build check
build:
	go build -o bin/sandbox ./cmd/sandbox
	go build -o bin/sandboxd ./cmd/sandboxd
	go build -o bin/sandbox-runner ./cmd/sandbox-runner

check:
	go build ./...
	go vet ./...
	go test ./...
	go test -race ./...

.PHONY: local-up local-forward local-down e2e
local-up:
	bash hack/local.sh up
local-forward:
	bash hack/local.sh forward
local-down:
	bash hack/local.sh down
e2e: build
	bash hack/local.sh test
