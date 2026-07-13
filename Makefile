BIN_DIR := bin
GO := go

.PHONY: all build test web-test jack-test clean

all: build

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/prtp-bridge ./cmd/prtp-bridge
	$(GO) build -o $(BIN_DIR)/prtp-matrix-helper ./cmd/prtp-matrix-helper

test:
	$(GO) test ./...

web-test:
	node --test scripts/rx_worklet_test.js

jack-test:
	KROMA_JACK_INTEGRATION=1 $(GO) test ./internal/prtpbridge/audio -run JACK -count=1 -v

clean:
	rm -rf $(BIN_DIR)
