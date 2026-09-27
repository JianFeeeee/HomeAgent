package core

import (
	"strings"
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// 阶段 1c：按 ToolDef.Parameters 预校验，在**分派之前**拦下坏参数。
//
// 现状：required 被 69 处声明却**无任何消费方**（grep 确认内核不读它），
// 校验散落在每个工具内部手写成中文字符串（"path is required"），
// 要等工具真被调用才暴露。
//
// ⚠️ 本判据的第一要务是**不误伤**：模型写错参数时内核要拦，但模型**写对**
// 的各种等价形态（"true" 当 bool、20 当 int）必须照常放行——
// 工具内部 getBool/getFloat 就是宽松解析的（见 utils.go 注释：
// "实际调用里三种都出现过"）。若校验比工具本身还严，会制造新失败。

// schemaWithRequired 造一个带 required 的参数 schema。
func schemaWithRequired(required []string, props map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// ① 缺 required 字段必须在**进分派前**被拦下，且指名字段。
func TestValidateArgsReportsMissingRequired(t *testing.T) {
	schema := schemaWithRequired([]string{"path", "content"},
		map[string]interface{}{
			"path":    map[string]interface{}{"type": "string"},
			"content": map[string]interface{}{"type": "string"},
		})
	cases := []struct {
		name      string
		args      map[string]interface{}
		wantField string
	}{
		{"两个都缺", map[string]interface{}{}, "path"},
		{"缺第二个", map[string]interface{}{"path": "/a"}, "content"},
		{"空字符串算缺失", map[string]interface{}{"path": "/a", "content": ""}, "content"},
		// ⚠️ 显式 null **不算**缺失（模型可能有意传 null，工具按零值处理）。
		// 真正的缺失是"键不存在"，由 required 列表表达。
		{"只传 content，path 键不存在", map[string]interface{}{"content": "x"}, "path"},
		// args 整个为 nil + schema 有 required ⇒ 等价于全部必填缺失。
		// （我最初把这条误放进「应放行」组——自相矛盾：组名是 no-constraints，
		//  而 schema 明明带了 required。写完立刻发现并改正。）
		{"args 为 nil", nil, "path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ve := validateToolArgs(c.args, schema)
			if ve == nil {
				t.Fatalf("缺 required 未被拦下: %#v", c.args)
			}
			if ve.Field != c.wantField {
				t.Errorf("Field = %q，期望 %q", ve.Field, c.wantField)
			}
			if ve.Reason != ErrReasonRequired {
				t.Errorf("Reason = %q，期望 %q", ve.Reason, ErrReasonRequired)
			}
		})
	}
}

// ② ★ 误伤防线：模型**实际会写**的等价形态必须放行。
// 这条是本阶段最大的回归风险——校验比工具更严就制造了新失败。
func TestValidateArgsAcceptsLenientEquivalentForms(t *testing.T) {
	schema := schemaWithRequired([]string{"name", "count", "flag", "items", "opts"},
		map[string]interface{}{
			"name":  map[string]interface{}{"type": "string"},
			"count": map[string]interface{}{"type": "integer"},
			"flag":  map[string]interface{}{"type": "boolean"},
			"items": map[string]interface{}{"type": "array"},
			"opts":  map[string]interface{}{"type": "object"},
		})
	// 这些形态在 getString/getBool/getFloat 宽松解析下**本来就可用**，
	// 若校验拒绝，就是内核自己制造失败。
	ok := []struct {
		name string
		args map[string]interface{}
	}{
		{"标准形态", map[string]interface{}{
			"name": "x", "count": 3, "flag": true,
			"items": []interface{}{"a"}, "opts": map[string]interface{}{"k": "v"},
		}},
		{"bool 传字符串 \"true\"", map[string]interface{}{
			"name": "x", "count": 3, "flag": "true",
			"items": []interface{}{"a"}, "opts": map[string]interface{}{},
		}},
		{"bool 传 \"1\"/\"0\"", map[string]interface{}{
			"name": "x", "count": 3, "flag": "0",
			"items": []interface{}{}, "opts": map[string]interface{}{},
		}},
		{"显式 null 视为已提供（不误伤）", map[string]interface{}{
			"name": "x", "count": 3, "flag": true,
			"items": []interface{}{}, "opts": nil,
		}},
		{"integer 传 float64（JSON 解码常态）", map[string]interface{}{
			"name": "x", "count": float64(3), "flag": false,
			"items": []interface{}{}, "opts": map[string]interface{}{},
		}},
		{"integer 传字符串 \"20\"（unitNumberRe 修过的形态）", map[string]interface{}{
			"name": "x", "count": "20", "flag": true,
			"items": []interface{}{}, "opts": map[string]interface{}{},
		}},
		{"字段名大小写/顺序不同", map[string]interface{}{
			"opts": map[string]interface{}{}, "items": []interface{}{},
			"flag": true, "count": 1, "name": "n",
		}},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if ve := validateToolArgs(c.args, schema); ve != nil {
				t.Errorf("**误伤**：本应放行却被拒: %v（args=%#v）", ve, c.args)
			}
		})
	}
}

// ③ 类型完全对不上时给出 type 错误（而不是放行到工具内部再报 xxx is required）。
func TestValidateArgsReportsTypeMismatch(t *testing.T) {
	schema := schemaWithRequired([]string{"name"},
		map[string]interface{}{
			"name": map[string]interface{}{"type": "string"},
		})
	// 传结构体当字符串：任何解析都不可能得到该值
	ve := validateToolArgs(map[string]interface{}{
		"name": map[string]interface{}{"nested": true},
	}, schema)
	if ve == nil {
		t.Fatal("类型完全不符未被拦下")
	}
	if ve.Reason != ErrReasonType {
		t.Errorf("Reason = %q，期望 %q", ve.Reason, ErrReasonType)
	}
	if ve.Field != "name" {
		t.Errorf("Field = %q，期望 name", ve.Field)
	}
}

// ④ 无 required / 无 schema 时一律放行（不因缺声明而阻塞任何工具）。
func TestValidateArgsPassesWhenNoConstraints(t *testing.T) {
	cases := []struct {
		name   string
		args   map[string]interface{}
		schema map[string]interface{}
	}{
		{"schema 为 nil", map[string]interface{}{"x": 1}, nil},
		{"schema 空表", map[string]interface{}{"x": 1}, map[string]interface{}{}},
		{"无 properties", map[string]interface{}{"x": 1}, map[string]interface{}{"type": "object"}},
		{"required 为空数组", map[string]interface{}{"x": 1}, schemaWithRequired([]string{}, map[string]interface{}{})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ve := validateToolArgs(c.args, c.schema); ve != nil {
				t.Errorf("无约束场景不应拦截，却得到: %v", ve)
			}
		})
	}
}

// ⑤ 错误文案必须**可执行**：含字段名、原因、以及改法。
// 这是「让模型看得懂真因」的核心——否则模型只会原样重试
// （实测 cmd_run 失败率 34%~48% 的成因）。
func TestValidateArgsErrorIsActionable(t *testing.T) {
	schema := schemaWithRequired([]string{"path"},
		map[string]interface{}{"path": map[string]interface{}{"type": "string", "description": "文件路径"}})
	ve := validateToolArgs(map[string]interface{}{}, schema)
	if ve == nil {
		t.Fatal("缺 required 未被拦下")
	}
	if ve.Hint == "" {
		t.Fatal("Hint 为空——模型将不知道该改什么，只会原样重试")
	}
	text := renderToolError("files_write", ve)
	for _, want := range []string{"files_write", "path"} {
		if !strings.Contains(text, want) {
			t.Errorf("错误文案缺少 %q: %s", want, text)
		}
	}
}

// 端到端：缺必填参数必须在**工具被调用之前**被拦下。
//
// 这是阶段 1c 的真正目标：此前 `required` 无消费方，坏参数要等工具真被
// 调用才报 "path is required" 这类与真因无关的错，模型据此只会原样重试。
// 本判据断言「设备真的没被调用」+「文案指名字段」两件事。
func TestSchemaValidationInterceptsBeforeDispatch(t *testing.T) {
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "tool_req", Arguments: map[string]interface{}{}}}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	var executed bool
	a.io.RegisterDevice(&schemaDevice{
		name: "schemadev", toolName: "tool_req",
		schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string", "description": "文件路径"}},
			"required":   []interface{}{"path"},
		},
		onExec: func() { executed = true },
	})

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}
	if executed {
		t.Error("缺必填参数仍进入了工具 —— 校验没有前置")
	}
	// 文案必须指名字段并给出改法，否则模型只会原样重试。
	var text string
	for _, m := range f.Msgs {
		if m.Role == "tool" {
			text = m.Content
		}
	}
	for _, want := range []string{"tool_req", "path", "必填"} {
		if !strings.Contains(text, want) {
			t.Errorf("错误文案缺少 %q: %s", want, text)
		}
	}
}

// 反向：参数**齐备**时必须照常执行（校验不得阻塞正常路径）。
func TestSchemaValidationPassesCompleteArgs(t *testing.T) {
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "tool_req2",
			Arguments: map[string]interface{}{"path": "/a/b.txt"}}}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	var executed bool
	a.io.RegisterDevice(&schemaDevice{
		name: "schemadev2", toolName: "tool_req2",
		schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
			"required":   []interface{}{"path"},
		},
		onExec: func() { executed = true },
	})

	if out := a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))); out != outcomeDone {
		t.Fatalf("runTaskSteps 未收敛: %v", out)
	}
	if !executed {
		t.Error("参数齐备却没执行 —— 校验误伤了正常路径")
	}
}

// schemaDevice 带 schema 声明的测试设备，并记录是否真被执行。
type schemaDevice struct {
	name     string
	toolName string
	schema   map[string]interface{}
	onExec   func()
}

func (d *schemaDevice) Name() string             { return d.name }
func (d *schemaDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *schemaDevice) Description() string      { return "schema test device" }
func (d *schemaDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{{Name: d.toolName, Parameters: d.schema}}
}
func (d *schemaDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	if d.onExec != nil {
		d.onExec()
	}
	return "ok", nil
}
func (d *schemaDevice) Start() error                                 { return nil }
func (d *schemaDevice) Stop() error                                  { return nil }
func (d *schemaDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *schemaDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
