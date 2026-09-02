package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// 端到端：用**真实 plugindev 模板**编译的插件，经内核 proc 通道加载运行。
//
// 与 plugin_test.go 中 testdata/*.go 假插件的区别：
// 那些是手写的最简 RPC 实现，只验证内核侧逻辑；
// 这里用的是 tools/plugindev/templates/proc_main.go.tmpl —— 外部插件作者
// 真正会拿到的那份运行时。它验证的是「模板 ↔ 内核」两侧协议/布局真的对齐，
// 而不只是内核自己跟自己对齐。
//
// 插件业务代码只用公开 SDK（NewPluginFactory + sdk.PluginSDK），与 .so 时代一致。

const e2ePluginSource = `package main

import (
	"strings"

	sdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type e2ePlugin struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &e2ePlugin{name: name}, nil
}

func (p *e2ePlugin) Name() string { return p.name }

func (p *e2ePlugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	s.RegisterTool("e2e_echo", sdk.ToolDef{
		Description: "回显",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{"type": "string"},
			},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		t, _ := args["text"].(string)
		return "echo:" + t, nil
	})

	s.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		for i := range ctx.ToolResults {
			if str, ok := ctx.ToolResults[i].Result.(string); ok {
				ctx.ToolResults[i].Result = strings.ReplaceAll(str, "脏", "净")
			}
		}
		// FinalText 在 C ABI 下对 after_toolcall 不可见（§8.3：10 → 16 字段）
		ctx.FinalText = ctx.FinalText + "|stage-touched"
		return nil
	})
	return nil
}

func (p *e2ePlugin) Stop() error { return nil }
`

// procRuntimeTemplates 列出 plugindev 会生成到插件目录的运行时文件。
//
// 必须与 SDK 仓 tools/plugindev/proc_runtime.go 的 procRuntimeFiles 一致：
// 共享段与事件通知的传递机制按平台不同（Unix 继承 fd，Windows 命名
// 内核对象），故拆成带 build tag 的文件；只写主模板会编译失败。
var procRuntimeTemplates = []struct {
	tmpl string
	out  string
}{
	{"proc_main.go.tmpl", "z_proc_gen.go"},
	{"proc_shm_unix.go.tmpl", "z_proc_shm_unix.go"},
	{"proc_shm_windows.go.tmpl", "z_proc_shm_windows.go"},
}

// buildPluginWithRealTemplate 用 plugindev 的真实模板编译一个插件二进制。
func buildPluginWithRealTemplate(t *testing.T, businessCode string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境无 go 工具链，跳过端到端测试")
	}

	tmplDir := filepath.Join("..", "..", "..",
		"third_party", "homeagent-sdk", "tools", "plugindev", "templates")

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "plugin.go"), businessCode)

	for _, rt := range procRuntimeTemplates {
		data, err := os.ReadFile(filepath.Join(tmplDir, rt.tmpl))
		if err != nil {
			t.Skipf("plugindev 模板 %s 不可读（SDK 仓可能未就位）: %v", rt.tmpl, err)
		}
		mustWriteFile(t, filepath.Join(dir, rt.out), string(data))
	}

	sdkPath, err := filepath.Abs(filepath.Join("..", "..", "..", "third_party", "homeagent-sdk"))
	if err != nil {
		t.Fatalf("解析 SDK 路径: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, "go.mod"),
		"module e2eplugin\n\ngo 1.25\n\n"+
			"require gitcode.com/JianFeeeee/homeagent-sdk v0.9.2\n\n"+
			"replace gitcode.com/JianFeeeee/homeagent-sdk => "+sdkPath+"\n")

	bin := filepath.Join(dir, "plugin.bin")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	// CGO_ENABLED=0：模板零 cgo 是迁移的核心收益，这里同时充当回归保护
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("用真实模板编译插件失败: %v\n%s", err, out)
	}
	return bin
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

// 完整链路：真实模板编译 → spawn → 握手 → init/start → 反向注册 → 工具调用 → stage 读改写。
func TestE2E_RealTemplatePluginFullLifecycle(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, e2ePluginSource)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	p := New("e2e", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	// 1) SetAutoRestart 必须经 lifecycle.autoRestart 上报到内核。
	//    公开 SDK 的 SetAutoRestart 是纯 setter（无 hook），插件在 Start() 里
	//    调它只改自己进程内的副本；模板须在 Start 返回后显式上报一次。
	if !core.autoStart {
		t.Error("插件的 SetAutoRestart(true) 未传达到内核（模板漏了 lifecycle.autoRestart 上报？）")
	}

	// 2) 工具注册与调用
	core.mu.Lock()
	toolHandler, hasTool := core.tools["e2e_echo"]
	core.mu.Unlock()
	if !hasTool {
		t.Fatal("插件注册的工具未到达内核")
	}
	res, err := toolHandler(map[string]interface{}{"text": "你好"})
	if err != nil {
		t.Fatalf("调用插件工具: %v", err)
	}
	if got, _ := res.(string); got != "echo:你好" {
		t.Errorf("工具返回 %q，期望 echo:你好", got)
	}

	// 3) stage 读改写经共享段回到内核 StageContext
	handlers := core.stageHandlers(pubsdk.StageAfterToolcall)
	if len(handlers) != 1 {
		t.Fatalf("应注册 1 个 after_toolcall handler，实际 %d", len(handlers))
	}

	sc := &pubsdk.StageContext{
		Phase:       pubsdk.StageAfterToolcall,
		FinalText:   "原文",
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Name: "t", Result: "这是脏数据"}},
	}
	if errs := runStageLikeKernel(handlers, sc); len(errs) > 0 {
		t.Fatalf("stage 执行失败: %v", errs)
	}

	got, _ := sc.ToolResults[0].Result.(string)
	if got != "这是净数据" {
		t.Errorf("清洗结果未回到内核 StageContext：实际 %q", got)
	}
	// FinalText 在 C ABI 的 after_toolcall 下根本看不到（只下发 10 字段中的一部分）
	if sc.FinalText != "原文|stage-touched" {
		t.Errorf("FinalText 改写未回传：实际 %q（C ABI 下此字段在本阶段不可见）", sc.FinalText)
	}
}

// 只读插件与改写插件并发时，改写结果不被覆盖。
//
// 这是本次迁移最关键的性质，用**真实模板**再验一次：
// 字段级脏写入使只读插件的写入集为空，物理上不可能覆盖他人改写。
// 对照 C ABI 副本模型实测 35.8~36.8% lost update（§8.4）。
func TestE2E_RealTemplateReadOnlyPluginDoesNotOverwrite(t *testing.T) {
	const readerSource = `package main

import sdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

type readerPlugin struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &readerPlugin{name: name}, nil
}

func (p *readerPlugin) Name() string { return p.name }

func (p *readerPlugin) Start(s *sdk.PluginSDK) error {
	// 只读：遍历但不改任何字段
	s.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		for range ctx.ToolResults {
		}
		_ = ctx.FinalText
		return nil
	})
	return nil
}

func (p *readerPlugin) Stop() error { return nil }
`

	writerBin := buildPluginWithRealTemplate(t, e2ePluginSource)
	readerBin := buildPluginWithRealTemplate(t, readerSource)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()

	// 两个插件共享同一 Host（= 同一 memfd）。
	// 若每插件一块段，这里就会退化成副本模型，本测试必然失败。
	writer := New("writer", writerBin, t.TempDir(), nil, host, nil)
	if err := writer.Start(core); err != nil {
		t.Fatalf("writer.Start: %v", err)
	}
	defer writer.Close()

	reader := New("reader", readerBin, t.TempDir(), nil, host, nil)
	if err := reader.Start(core); err != nil {
		t.Fatalf("reader.Start: %v", err)
	}
	defer reader.Close()

	handlers := core.stageHandlers(pubsdk.StageAfterToolcall)
	if len(handlers) != 2 {
		t.Fatalf("应有 2 个 after_toolcall handler，实际 %d", len(handlers))
	}

	sc := &pubsdk.StageContext{
		Phase:       pubsdk.StageAfterToolcall,
		FinalText:   "原文",
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Name: "t", Result: "这是脏数据"}},
	}
	if errs := runStageLikeKernel(handlers, sc); len(errs) > 0 {
		t.Fatalf("stage 执行失败: %v", errs)
	}

	got, _ := sc.ToolResults[0].Result.(string)
	if got != "这是净数据" {
		t.Fatalf("只读插件覆盖了改写插件的结果（lost update）：实际 %q", got)
	}
	if !strings.Contains(sc.FinalText, "stage-touched") {
		t.Errorf("FinalText 改写被覆盖：实际 %q", sc.FinalText)
	}
}
