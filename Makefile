VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet image run-baseline clean

build: ## build ./bin/aoc
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/aoc ./cmd/aoc

test:
	go test ./...

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | test -z "$$(cat)"

image: ## build the docker image used by the compose files
	docker compose build

run-baseline: build ## the smallest end-to-end experiment (needs docker)
	./bin/aoc run --profile profiles/baseline.yaml --name baseline

clean:
	rm -rf bin results
