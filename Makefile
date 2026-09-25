.PHONY: all build build-plain build-cli build-gui clean install test run build-static build-linux-arm64 lint fmt sync-client-versions check-client-versions csrc csrc-test
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
#
# 注意：内核 C 编解码层（internal/agent/api/ha_codec.c）**不靠 tag 开关**，
# 而是由 cgo 本身决定（`//go:build cgo` / `!cgo`）。原因：它是零依赖纯 C99
# 源码内联编译，不需要任何外部库/工具链前提；而 homed 本就强制 cgo
# （sqlite3 + gojieba），所以 C 路径自然生效，无需额外开关。
# 对比 onnxruntime：那个需要运行期的 libonnxruntime.so，所以必须显式 tag。
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

# csrc：C 源码的**独立**产物（静态库 + 契约测试），供 C 侧复用（鸿蒙/嵌入式/C SDK）。
#
# ⚠️ Go 构建**不依赖**它：internal/agent/api/ha_codec.{c,h} 是指向 csrc/ 的
# **符号链接**，cgo 直接编这份源码，而不是链接预构建的 .a。
#
# 为什么是「包内符号链接」而不是别的（历史教训 + 实测，勿回退）：
#   1. 不能链静态库：.a 是构建产物、不入库，而发布脚本原先不产出它
#      ⇒「不入库 + 不生成」两头空（实测 cannot find csrc/build/libha_codec.a）；
#      且交叉编译 linux/arm64 时宿主 x86-64 的 .a 被链进目标产物，
#      报 `file in wrong format`。
#   2. 不能用 `#include "../../../csrc/src/ha_codec.c"`（包外相对包含）：
#      ★ Go 构建缓存**不跟踪包外被 #include 的 C 文件**，改了 C 源码但缓存命中时
#        会静默沿用旧代码（实测：变异 C 源码后 go test 仍报 ok）。
#        这对「逐步推进 C 化」是致命的——改动无效却无人察觉。
#   3. 包内符号链接：文件在包目录内 ⇒ 缓存按内容哈希正确跟踪
#      （实测：改 csrc/ 源文件后 go test 立即判红）；
#      同时只有一份权威源（csrc/），无副本漂移、无需同步目标。
#
# csrc 目标本身只服务「C 侧独立使用 + ctest」，不参与 Go 构建链路。
CSRC_DIR=csrc
CSRC_BUILD=$(CSRC_DIR)/build
CSRC_LIB=$(CSRC_BUILD)/libha_codec.a

csrc: $(CSRC_LIB)

$(CSRC_LIB): $(wildcard $(CSRC_DIR)/src/*.c) $(wildcard $(CSRC_DIR)/include/*.h) $(CSRC_DIR)/CMakeLists.txt
	@cmake -S $(CSRC_DIR) -B $(CSRC_BUILD) -DBUILD_TESTS=ON >/dev/null
	@cmake --build $(CSRC_BUILD) -j >/dev/null
	@echo "Built: $(CSRC_LIB)（C 侧复用，不参与 Go 构建）"

# csrc-test：C 侧契约测试（黄金对照的另一半，见 docs/zh/c-core/llm-orchestration-c.md §五）
csrc-test: csrc
	@cd $(CSRC_BUILD) && ctest --output-on-failure

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build $(TAG_ARGS) -trimpath -installsuffix dynlink -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) ./cmd/homed/
	@echo "Built: $(BUILD_DIR)/$(BINARY) ($(VERSION), tags='$(HOMED_TAGS)')"
	@go version -m $(BUILD_DIR)/$(BINARY) | grep -q 'onnxruntime' \
		|| echo "WARN: 本次构建不含 onnxruntime，本地向量空间不可用（HOMED_TAGS= 显式关掉时才符合预期）"
	@go version -m $(BUILD_DIR)/$(BINARY) | grep -q 'CGO_ENABLED=1' \
		|| echo "WARN: 本次构建未启用 cgo，编解码走纯 Go 回退（不应发生）"

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

# build-linux-arm64：交叉编译 homed（真实发布目标之一）。
#
# 两处必须显式给定，否则必然失败（都不是 C 化引入的，但都长期缺覆盖）：
#   1. CC/CXX 交叉工具链。缺 CXX 时 cgo 回退到宿主 g++，而宿主编译器不认
#      aarch64 汇编，报 `gcc_arm64.S: no such instruction: 'stp x29,x30,[sp,'`。
#      deploy/packaging/build.sh:49 一直是对的，此处此前漏了。
#   2. .syso 隔离。cmd/{homed,waiter}/*.syso 是 Windows COFF 资源对象，
#      Go 会把同目录 .syso **无条件**链进任何目标；交叉到非 Windows 平台报
#      `file format not recognized`。build.sh 有 hide_syso_for_target，此处同样漏了。
CC_ARM64 ?= aarch64-linux-gnu-gcc
CXX_ARM64 ?= aarch64-linux-gnu-g++

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	@for f in cmd/homed/*.syso cmd/waiter/*.syso; do \
		[ -f "$$f" ] || continue; \
		mv "$$f" "$$f.hidden"; \
	done; \
	trap 'for f in cmd/homed/*.syso.hidden cmd/waiter/*.syso.hidden; do \
		[ -f "$$f" ] || continue; mv "$$f" "$${f%.hidden}"; done' EXIT; \
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_ARM64) CXX=$(CXX_ARM64) \
		$(GO) build $(TAG_ARGS) -installsuffix dynlink -ldflags '$(LDFLAGS)' \
		-o $(BUILD_DIR)/$(BINARY)-arm64 ./cmd/homed/; \
	for f in cmd/homed/*.syso.hidden cmd/waiter/*.syso.hidden; do \
		[ -f "$$f" ] || continue; mv "$$f" "$${f%.hidden}"; done; \
	trap - EXIT
	@echo "Built (arm64): $(BUILD_DIR)/$(BINARY)-arm64 ($$(file $(BUILD_DIR)/$(BINARY)-arm64 | sed 's/.*: //'))"

clean:
	rm -rf $(BUILD_DIR) $(BINARY) $(CSRC_BUILD)

install: build
	-systemctl stop homeagent 2>/dev/null
	cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	mkdir -p /etc/homeagent /var/lib/homeagent
	cp deploy/homeagent.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "Installed. Run: systemctl enable --now homeagent"

test:
	$(GO) test ./...
	@$(MAKE) csrc-test
	@$(MAKE) check-codec-cgo-only

# check-codec-cgo-only：钉死「编解码层完全 C 化」这一决定。
#
# 两条断言，缺一不可：
#   ① CGO_ENABLED=1 下测试全绿（含黄金对照：C 与纯 Go 参考实现逐值相等）
#   ② CGO_ENABLED=0 下**构建必须失败**
#
# 为什么②要断言「失败」而不是「也能编过」：内核已完全 C 化，C 是唯一实现。
# 若有人在 CGO_ENABLED=0 下让整包静默编过（例如加回一个纯 Go 回退），
# 就会同时存在两份语义可能分叉的实现 —— 而 C 侧对畸形 UTF-8 的解码边界
# 一旦与 Go 分叉，只表现为 rune 计数偏差（进而 token 预算与截断点偏移），
# **不会立刻暴露**。所以这里把「不许有第二条路」变成可执行的断言。
#
# 注：这不影响任何现有构建 —— waiter/initconfig/memgc 均不依赖本包
# （go list -deps 实测）；homed 本就强制 cgo。
.PHONY: check-codec-cgo-only
check-codec-cgo-only:
	@echo "== 编解码层：完全 C 化检查 =="
	@CGO_ENABLED=1 $(GO) test -count=1 ./internal/agent/api/ \
		&& echo "  ① cgo 下测试全绿（含黄金对照）: OK"
	@if CGO_ENABLED=0 $(GO) build ./internal/agent/api/ 2>/dev/null; then \
		echo "  [FAIL] CGO_ENABLED=0 下本包竟然构建成功——"; \
		echo "         编解码层已完全 C 化，不该存在第二条实现路径。"; \
		echo "         若是有意引入回退，请同时更新本检查与 codec_cgo.go 的说明。"; \
		exit 1; \
	else \
		echo "  ② CGO_ENABLED=0 下响亮失败（防静默回退）: OK"; \
	fi

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
