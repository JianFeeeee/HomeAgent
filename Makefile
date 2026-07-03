.PHONY: all build clean install test run build-cli

BINARY=homed
CLI_BINARY=homecli
GO=go
GOCACHE=/tmp/gocache
GOPATH=$(shell go env GOPATH)
BUILD_DIR=build

all: build build-cli

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build -o $(BUILD_DIR)/$(BINARY) ./cmd/homed/
	@echo "Built: $(BUILD_DIR)/$(BINARY)"

build-cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -o $(BUILD_DIR)/$(CLI_BINARY) ./cmd/cli/
	@echo "Built: $(BUILD_DIR)/$(CLI_BINARY)"

build-static:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build -tags netgo -ldflags '-extldflags "-static"' -o $(BUILD_DIR)/$(BINARY)-static ./cmd/homed/
	@echo "Built (static): $(BUILD_DIR)/$(BINARY)-static"

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 $(GO) build -o $(BUILD_DIR)/$(BINARY)-arm64 ./cmd/homed/
	@echo "Built (arm64): $(BUILD_DIR)/$(BINARY)-arm64"

clean:
	rm -rf $(BUILD_DIR) $(BINARY)

install: build
	cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	mkdir -p /etc/homeagent /var/lib/homeagent
	cp config/config.yaml /etc/homeagent/
	cp deploy/homeagent.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "Installed. Run: systemctl enable --now homeagent"

test:
	$(GO) test ./...

run: build
	./$(BUILD_DIR)/$(BINARY) -data /tmp/homeagent

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...
