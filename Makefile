# sipclient -- CLI SIP client with two channels for warm transfer.
# See README.md for usage and requirements.md for the specification.

BINARY      := sipclient
VERSION     ?= 1.0.0
CONFIG      ?= config.json

# CGO is required: the audio backend (miniaudio via malgo) needs it to reach
# the microphone and speakers. Building without it degrades to silent
# operation, which the -no-audio target below does deliberately instead.
export CGO_ENABLED := 1

GOFILES     := $(shell find . -name '*.go' -not -path './vendor/*')
LDFLAGS     := -X main.version=$(VERSION)

# Directory for test logs and SIP traces when running `make test-keep`.
TEST_DIR    ?= ./testruns

.DEFAULT_GOAL := build

## build: compile the client binary
.PHONY: build
build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .
	@echo "built ./$(BINARY) (version $(VERSION), CGO_ENABLED=$(CGO_ENABLED))"

## run: build then start the client interactively
.PHONY: run
run: build
	./$(BINARY) -config $(CONFIG)

## run-no-audio: start without opening an audio device (signalling only)
.PHONY: run-no-audio
run-no-audio: build
	./$(BINARY) -config $(CONFIG) -no-audio

## test: run unit and scenario tests
.PHONY: test
test:
	go test ./... -timeout 600s

## test-verbose: run tests with per-test output
.PHONY: test-verbose
test-verbose:
	go test ./... -v -timeout 600s

## test-keep: run tests and keep each scenario's app log and SIP trace in TEST_DIR
.PHONY: test-keep
test-keep:
	@mkdir -p $(TEST_DIR)
	SIPCLIENT_TEST_DIR=$(abspath $(TEST_DIR)) go test ./... -timeout 600s
	@echo "logs and SIP traces kept in $(TEST_DIR)/<TestName>/"

## test-unit: run only the fast in-process tests, skipping the scenario suite
.PHONY: test-unit
test-unit:
	go test ./internal/... -timeout 120s

## race: run every test with the client binary itself race-instrumented
.PHONY: race
race:
	SIPCLIENT_TEST_RACE=1 go test -race ./... -timeout 900s

## vet: run go vet
.PHONY: vet
vet:
	go vet ./...

## fmt: format all Go source
.PHONY: fmt
fmt:
	gofmt -w $(GOFILES)

## fmt-check: fail if any file is not gofmt-clean
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l $(GOFILES)); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "gofmt: clean"

## check: everything CI should run -- acceptance criterion A18
.PHONY: check
check: fmt-check vet build race
	@echo "all checks passed"

## tidy: prune and verify module dependencies
.PHONY: tidy
tidy:
	go mod tidy
	go mod verify

## config: create config.json from the template if it does not exist
.PHONY: config
config:
	@if [ -f $(CONFIG) ]; then \
		echo "$(CONFIG) already exists, leaving it alone"; \
	else \
		cp config.json.example $(CONFIG) && chmod 600 $(CONFIG) && \
		echo "created $(CONFIG) (mode 600) -- set sip.username, sip.domain and sip.server.host"; \
	fi

## clean: remove the binary, logs and kept test output
.PHONY: clean
clean:
	rm -f $(BINARY) sipclient.log sip-trace.log
	rm -rf $(TEST_DIR)
	@echo "cleaned"

## help: list the available targets
.PHONY: help
help:
	@echo "sipclient -- available targets:"
	@echo
	@grep -hE '^## [a-z-]+:' $(MAKEFILE_LIST) \
		| sed -e 's/^## //' \
		| awk -F': *' '{printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "variables: VERSION=$(VERSION) CONFIG=$(CONFIG) TEST_DIR=$(TEST_DIR)"
