.PHONY: all build build-plain build-cli build-gui clean install test run build-static build-linux-arm64 lint fmt sync-client-versions check-client-versions

# HOMED_TAGS 默认带 onnxruntime：发行版**默认启用**本地向量空间（与
# deploy/packaging/build.sh 保持一致）。
#
# 曾经这里是空 tags，实测的后果（2026-09-15 热部署）：`make build` 产出的
# homed 只有 33MB，而 onnxruntime 版是 84MB；启动日志里
# 「multimodal space active: provider=chineseclip」整行消失，少加载一个插件，
# 静态词向量也退化成 fallback——而打包脚本会直接**拒收**这种二进制
# （package-linux.sh 检查 `-tags=.*onnxruntime`）。即「本地随手 make build」
# 与「发行构建」不是同一个东西，部署时无从察觉。
# 需要极简构建时显式 HOMED_TAGS= 关掉。
HOMED_TAGS ?= onnxruntime
TAG_ARGS = $(if $(HOMED_TAGS),-tags $(HOMED_TAGS),)

BINARY=homed
CLI_BINARY=waiter
GUI_BINARY=homeagent-gui
GO=go
GOCACHE=/tmp/gocache
export GOPATH=/tmp/gopath
BUILD_DIR=build
PROJECT_ROOT := $(CURDIR)
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo "0.8.0")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS = -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Version=$(VERSION) -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Commit=$(COMMIT) -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.BuildTime=$(BUILD_TIME)

all: build build-cli

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build $(TAG_ARGS) -trimpath -installsuffix dynlink -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) ./cmd/homed/
	@echo "Built: $(BUILD_DIR)/$(BINARY) ($(VERSION), tags='$(HOMED_TAGS)')"
	@go version -m $(BUILD_DIR)/$(BINARY) | grep -q 'onnxruntime' \
		|| echo "WARN: 本次构建不含 onnxruntime，本地向量空间不可用（HOMED_TAGS= 显式关掉时才符合预期）"

build-cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -installsuffix dynlink -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(CLI_BINARY) ./cmd/waiter/
	@echo "Built: $(BUILD_DIR)/$(CLI_BINARY) ($(VERSION))"

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

# 客户端版本与内核版本同步（唯一事实源 internal/meta.Version）。
# GUI/鸿蒙各有自版本字段，手工改必漂——用脚本拉齐，check 版给门禁用。
sync-client-versions:
	@bash deploy/scripts/sync-client-versions.sh

check-client-versions:
	@bash deploy/scripts/sync-client-versions.sh --check

# lint-full：在 vet 之外跑 golangci-lint（阈值见 .golangci.yml，起步 warn-only）。
# 未安装时给出可执行的安装提示与跳过原因，而不是静默成功。
.PHONY: lint-full
lint-full:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint 未安装，跳过（阈值见 .golangci.yml）"; \
		echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi
