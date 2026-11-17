export GOTOOLCHAIN := local
export GOPROXY := off
export GOSUMDB := off

.PHONY: build check deploy-check local-up local-port-forward local-check local-stop local-down site site-check
build:
	go build -o bin/sandbox ./cmd/sandbox
	go build -o bin/sandboxd ./cmd/sandboxd
	go build -o bin/sandbox-runner ./cmd/sandbox-runner
	go build -o bin/sandbox-bench ./cmd/sandbox-bench

check:
	go build ./...
	go vet ./...
	go test ./...
	go test -race ./...
	$(MAKE) deploy-check

deploy-check:
	python3 -m unittest discover -s deploy -p 'test_*.py'

local-up local-port-forward local-check local-stop local-down:
	python3 deploy/local/local.py $(patsubst local-%,%,$@) $(if $(filter yes,$(CONFIRM)),--confirm,)

site:
	python3 -m http.server 3000 --bind 127.0.0.1 --directory website

site-check:
	node --test website/test.mjs
