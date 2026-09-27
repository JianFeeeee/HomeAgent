package seq

import (
	"encoding/json"
	"strings"
	"testing"
)

// 阶段 P1：序列文本 → AST 的解析与静态校验。
//
// 判据优先原则：先写、必须能失败、再实现。
//
// 本文件钉死四条最容易出错、且出错后**静默**的地方：
//  1. `tools` 是**字符串**，`;` 分隔的每个 `{…}` 才是分隔单位
//  2. 分隔符 `;` 只在 brace 深度 0 且不在字符串内时才算
//  3. 漏 `;` / 漏末尾 `;` / 连续 `;` 必须**硬报错**，不得静默吞工具
//  4. 具名槽校验：`as` 必须在 `out` 声明，`$args.` 必须在 `in` 声明

// validSeq 是一条合法的序列文本（引用来源见 tests/fixtures）。
const validSeq = `{
  "name": "巡检三节点",
  "description": "并行拉取三台节点状态",
  "groups": [
    {
      "name": "拉取单台",
      "description": "拉取一台节点的 uptime",
      "in":  { "host": "string" },
      "out": { "summary": "string" },
      "tools": "{\"tool\":\"cmd_run\",\"args\":{\"command\":\"uptime\"},\"as\":\"summary\"} ;"
    }
  ]
}`

// ① 合法文本必须解析成功。
func TestParseValidSequence(t *testing.T) {
	seq, err := Parse([]byte(validSeq))
	if err != nil {
		t.Fatalf("合法序列解析失败: %v", err)
	}
	if seq.Name != "巡检三节点" {
		t.Errorf("name = %q", seq.Name)
	}
	if len(seq.Groups) != 1 {
		t.Fatalf("groups = %d，期望 1", len(seq.Groups))
	}
	g := seq.Groups[0]
	if g.Name != "拉取单台" {
		t.Errorf("组名 = %q", g.Name)
	}
	if len(g.Tools) != 1 {
		t.Fatalf("组内工具 = %d，期望 1", len(g.Tools))
	}
	if g.Tools[0].Tool != "cmd_run" {
		t.Errorf("工具名 = %q", g.Tools[0].Tool)
	}
	if g.Tools[0].As != "summary" {
		t.Errorf("as = %q，期望 summary", g.Tools[0].As)
	}
}

// ② ★ `;` 必须在字符串内不生效。
//
// 真实 fixture：模型的 command 参数里大量含分号（取自仓库
// stream_accumulate_test.go 的线上日志原文）。被 `{…}` 包裹后，
// 命令里的分号在**字符串内** ⇒ 切分器不得碰它。
func TestParseToolCommandContainingSemicolons(t *testing.T) {
	// 命令含 5 个分号，且含转义引号
	// ★ 用 json.Marshal **分层构造**，不用手写多层转义。
	//
	// 为什么必须这样：tools 的值是一段"内含引号的 JSON 文本"，它本身还要被
	// 放进外层 JSON 字符串里 ⇒ 两层转义。手写转义时我曾把内层引号写成了
	// \"（一层），导致分词器永远进不了字符串态 —— 判据成了摆设
	//（实测：删掉分词器的字符串跟踪，本用例仍全绿）。
	// 用 Marshal 构造就没有手写出错的机会。
	command := `echo "=== raw ==="; cat /tmp/x.json; echo; echo "=== ps ==="; ` +
		`ps aux | grep -c "[r]un.py"; echo "=== log ==="; cat /tmp/run.log`
	toolJSON, err := json.Marshal(map[string]interface{}{
		"tool": "cmd_run",
		"args": map[string]interface{}{"command": command, "timeout": "30s"},
		"as":   "summary",
	})
	if err != nil {
		t.Fatalf("构造 fixture 失败: %v", err)
	}
	// 组文本：内层用 `;` 分隔的两个结构，第二个故意也含分号
	toolsStr := string(toolJSON) + " ;"
	groupText, err := json.Marshal(map[string]interface{}{
		"name":  "g",
		"in":    map[string]string{},
		"out":   map[string]string{"summary": "string"},
		"tools": toolsStr,
	})
	if err != nil {
		t.Fatalf("构造 group 失败: %v", err)
	}
	seqText := `{"name":"t","groups":[` + string(groupText) + `]}`

	seq, err := Parse([]byte(seqText))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	tools := seq.Groups[0].Tools
	if len(tools) != 1 {
		t.Fatalf("含 5 个分号的命令被切成了 %d 个工具（期望 1）", len(tools))
	}
	cmd, _ := tools[0].Args["command"].(string)
	for _, want := range []string{"cat /tmp/x.json", "ps aux", "cat /tmp/run.log"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("命令被截断了，缺少 %q: %q", want, cmd)
		}
	}
	// 分号必须**原样**保留在 command 里
	if cmd != command {
		t.Errorf("命令被破坏:\n得到 %q\n期望 %q", cmd, command)
	}
}

// ③ ★ 漏分隔符 / 漏末尾 / 连续分号：必须硬报错。
//
// 这一条是「静默吞工具」的防线：若漏一个 `;` 却不报错，序列会少执行
// 一个工具，而模型以为跑完了 —— 与本仓反复吃亏的「静默降级」同族。
func TestParseRejectsMalformedToolSeparators(t *testing.T) {
	cases := []struct {
		name    string
		tools   string
		wantSub string
	}{
		{"漏中间分隔符",
			`{"tool":"cmd_run","args":{"command":"a"},"as":"x"} {"tool":"cmd_run","args":{"command":"b"},"as":"y"} ;`,
			";"},
		{"漏末尾分号",
			`{"tool":"cmd_run","args":{"command":"a"},"as":"x"}`,
			";"},
		{"连续分号",
			`{"tool":"cmd_run","args":{"command":"a"},"as":"x"} ; ; {"tool":"cmd_run","args":{"command":"b"},"as":"y"} ;`,
			"空"},
		{"结构未闭合",
			`{"tool":"cmd_run","args":{"command":"a"},"as":"x"`,
			"闭合"},
		{"空 tools",
			``,
			"工具"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := `{"name":"t","groups":[{"name":"g","in":{},"out":{"x":"string","y":"string"},"tools":` +
				strconvQuote(c.tools) + `}]}`
			if _, err := Parse([]byte(text)); err == nil {
				t.Fatalf("畸形 tools 未被拒绝: %q", c.tools)
			} else if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误信息应提到 %q，实际: %v", c.wantSub, err)
			}
		})
	}
}

// ④ 具名槽校验：`as` 必须在 `out` 声明过。
// 这是具名槽设计的**全部价值所在**——写错能在构建期发现。
func TestParseRejectsUndeclaredOutSlot(t *testing.T) {
	text := `{"name":"t","groups":[{"name":"g","in":{},"out":{"summary":"string"},` +
		`"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"a\"},\"as\":\"not_declared\"} ;"}]}`
	if _, err := Parse([]byte(text)); err == nil {
		t.Fatal("as 指向未声明的 out 槽却通过了 —— 具名槽失去意义")
	}
}

// ⑤ `$args.` 必须在 `in` 声明过。
func TestParseRejectsUndeclaredArg(t *testing.T) {
	text := `{"name":"t","groups":[{"name":"g","in":{"host":"string"},"out":{"summary":"string"},` +
		`"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"$args.other\"},\"as\":\"summary\"} ;"}]}`
	if _, err := Parse([]byte(text)); err == nil {
		t.Fatal("引用了未声明的入参却通过了")
	}
}

// ⑥ 组名重复必须报错（它是签名名，必须唯一才能按名调用）。
func TestParseRejectsDuplicateGroupName(t *testing.T) {
	text := `{"name":"t","groups":[` +
		`{"name":"g","in":{},"out":{"x":"string"},"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"a\"},\"as\":\"x\"} ;"},` +
		`{"name":"g","in":{},"out":{"y":"string"},"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"b\"},\"as\":\"y\"} ;"}]}`
	if _, err := Parse([]byte(text)); err == nil {
		t.Fatal("组名重复却通过了 —— 按名调用会歧义")
	}
}

// ⑦ 非 array 槽被同名 as 写多次必须报错（组内并行 ⇒ 数据竞争）。
func TestParseRejectsDuplicateAsOnScalarSlot(t *testing.T) {
	text := `{"name":"t","groups":[{"name":"g","in":{},"out":{"x":"string"},` +
		`"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"a\"},\"as\":\"x\"} ; {\"tool\":\"cmd_run\",\"args\":{\"command\":\"b\"},\"as\":\"x\"} ;"}]}`
	if _, err := Parse([]byte(text)); err == nil {
		t.Fatal("标量槽被同名 as 写两次却通过了")
	}
}

// ⑧ 反例：array 槽允许同名 as（在组屏障按顺序追加）。
func TestParseAllowsDuplicateAsOnArraySlot(t *testing.T) {
	text := `{"name":"t","groups":[{"name":"g","in":{},"out":{"xs":"array"},` +
		`"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"a\"},\"as\":\"xs\"} ; {\"tool\":\"cmd_run\",\"args\":{\"command\":\"b\"},\"as\":\"xs\"} ;"}]}`
	if _, err := Parse([]byte(text)); err != nil {
		t.Fatalf("array 槽的同名 as 应被允许: %v", err)
	}
}

// strconvQuote 用 JSON 字符串字面量包裹一段文本。
func strconvQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// ②' ★ 隔离「字符串内不参与深度计算」这条规则。
//
// 前一条 fixture（TestParseToolCommandContainingSemicolons）**验证不到**
// 这条规则：它的分号落在 args 对象的花括号内部，depth > 0 就足以保护，
// 即使分词器完全不懂字符串也能切对。实测删掉字符串跟踪后该用例仍全绿。
//
// 本条用**字符串里的花括号**做判别：command 的值里含 `{` 与 `}`，它们在
// JSON 字符串内，不得影响 depth。若不跟踪字符串态，这些花括号会把 depth
// 推离 0，随后的 ';' 就被当成顶层分隔符 ⇒ 工具被切碎。
//
// ⚠️ 必须用 json.Marshal 构造内层 JSON：手写转义会把引号写成 \"，
// 于是字符串里根本没有未转义引号，分词器永远进不了字符串态——
// 判据就成了摆设（这一点我踩过两次）。
func TestBracesInsideStringDoNotAffectDepth(t *testing.T) {
	// ⚠️ 花括号必须**不成对**（这里只有一个 '{'）。这是本判据能隔离规则的
	// 唯一原因：成对时（"echo {x; y}"）删掉字符串跟踪也能切对 —— depth 被
	// 推高又回落，净效果为零，判据形同虚设（我第一次就是这么写的，变异测不出来）。
	// 不成对时，字符串外的分词器会被 depth 卡住无法归零 ⇒ 必然报错。
	// 场景取自实际用法：grep 统计字面量花括号。
	command := "grep -o '{' /etc/hosts"
	toolJSON, err := json.Marshal(map[string]interface{}{
		"tool": "cmd_run",
		"args": map[string]interface{}{"command": command},
		"as":   "summary",
	})
	if err != nil {
		t.Fatalf("构造 fixture 失败: %v", err)
	}
	groupText, err := json.Marshal(map[string]interface{}{
		"name":  "g",
		"in":    map[string]string{},
		"out":   map[string]string{"summary": "string"},
		"tools": string(toolJSON) + " ;",
	})
	if err != nil {
		t.Fatalf("构造 group 失败: %v", err)
	}
	seqText := `{"name":"t","groups":[` + string(groupText) + `]}`

	seq, err := Parse([]byte(seqText))
	if err != nil {
		t.Fatalf("字符串内的 {} ; 影响了分词: %v", err)
	}
	if len(seq.Groups[0].Tools) != 1 {
		t.Fatalf("应解析为 1 个工具，实际 %d —— 字符串内的分号被切开了", len(seq.Groups[0].Tools))
	}
	got, _ := seq.Groups[0].Tools[0].Args["command"].(string)
	if got != command {
		t.Errorf("command 被破坏: %q（期望 %q）", got, command)
	}
}
