.PHONY: all build build-cli build-gui clean install test run build-static build-linux-arm64 lint fmt

BINARY=homed
CLI_BINARY=waiter
GUI_BINARY=homeagent-gui
GO=go
GOCACHE=/tmp/gocache
export GOPATH=/tmp/gopath
BUILD_DIR=build
PROJECT_ROOT := $(CURDIR)
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo "0.7.1")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS = -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Version=$(VERSION) -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Commit=$(COMMIT) -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.BuildTime=$(BUILD_TIME)

all: build build-cli

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build -trimpath -installsuffix dynlink -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) ./cmd/homed/
	@echo "Built: $(BUILD_DIR)/$(BINARY) ($(VERSION))"

build-cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -installsuffix dynlink -o $(BUILD_DIR)/$(CLI_BINARY) ./cmd/waiter/
	@echo "Built: $(BUILD_DIR)/$(CLI_BINARY)"

build-gui:
	@cd cmd/gui && npm install --production && npx electron-packager . $(GUI_BINARY) --out=../../$(BUILD_DIR) --overwrite --no-sandbox
	@echo "Built: $(BUILD_DIR)/$(GUI_BINARY)"

build-static:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build -tags netgo -installsuffix dynlink -ldflags '-extldflags "-static" $(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY)-static ./cmd/homed/
	@echo "Built (static): $(BUILD_DIR)/$(BINARY)-static"

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 $(GO) build -installsuffix dynlink -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY)-arm64 ./cmd/homed/
	@echo "Built (arm64): $(BUILD_DIR)/$(BINARY)-arm64"

clean:
	rm -rf $(BUILD_DIR) $(BINARY)

install: build
	-systemctl stop homeagent 2>/dev/null
	cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	mkdir -p /etc/homeagent /var/lib/homeagent
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
