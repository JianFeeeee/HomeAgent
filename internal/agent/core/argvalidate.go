package core

import (
	"fmt"
	"strconv"
	"strings"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// 阶段 1c：按 ToolDef.Parameters 预校验。
//
// 存在的理由：`required` 在仓内被声明了 69 处，却**没有任何消费方**
// （内核从不读它）。校验散落在每个工具内部手写成中文字符串
// （"path is required"），要等工具**真被调用**才暴露——而模型看到这类
// 与真因无关的报错只会原样重试（实测 cmd_run 失败率 34%~48% 的成因）。
//
// ⚠️ 第一要务是**不误伤**。工具内部的 getter 是宽松解析的
// （见 utils.go：`getBool` 注释写明"实际调用里 bool/string/float 三种都出现过"，
//
//	`getFloat` 接受 int 与 float64）。若校验比工具本身更严，
//
// 就是内核自己制造新的失败——那比不校验更糟。
// 因此本校验器**只拦真正无法解析的形态**，对宽松等价形态一律放行。
func validateToolArgs(args map[string]interface{}, schema map[string]interface{}) *sdk.ToolError {
	if schema == nil {
		return nil
	}
	props, _ := schema["properties"].(map[string]interface{})
	required := schemaRequired(schema)

	// ① required 检查：**键必须存在**，且值不得是空字符串。
	//    ⚠️ 判据是「键的存在性」而非「值是否为 nil」——显式 null 是模型
	//    有意传的零值，不能当缺失；而键真的没传才是缺失。
	for _, name := range required {
		if name == "" {
			continue
		}
		v, present := args[name]
		if !present || isBlankArg(v) {
			return newToolError(ErrReasonRequired, name,
				fmt.Sprintf("缺少必填参数 %s", name),
				requiredHint(name, props[name]))
		}
	}

	// ② 类型检查：只对**已提供**的 required 字段做，且只拦真正对不上的。
	for _, name := range required {
		if name == "" {
			continue
		}
		v, present := args[name]
		if !present {
			continue // 已在 ① 报过
		}
		if ve := checkArgType(v, propType(props[name])); ve != nil {
			ve.Field = name
			return ve
		}
	}
	return nil
}

// schemaRequired 取出 required 列表，两种声明形态都认。
func schemaRequired(schema map[string]interface{}) []string {
	switch v := schema["required"].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// propType 取某个属性的声明类型（没有 properties 时返回空串 = 不检查）。
func propType(prop interface{}) string {
	m, ok := prop.(map[string]interface{})
	if !ok {
		return ""
	}
	t, _ := m["type"].(string)
	return t
}

// isBlankArg 报告一个**已提供**的值是否为空（只有空字符串算）。
//
// nil 不在此判定：显式 null 是模型有意传的零值，工具侧按零值处理
// （getString→""、getBool→false、map 取键→nil），把它当缺失会误伤。
// 真正的「没传」由 required 检查里的**键存在性**判定，不靠值。
func isBlankArg(v interface{}) bool {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// requiredHint 为缺失的必填参数生成**可执行**的改法。
// 带上属性描述——那是作者写给模型的说明，比"参数不能为空"有用得多。
func requiredHint(name string, prop interface{}) string {
	desc := ""
	if m, ok := prop.(map[string]interface{}); ok {
		desc, _ = m["description"].(string)
	}
	if desc != "" {
		return fmt.Sprintf("请补上 %s 参数（%s）。该参数为必填，"+
			"不要重复本次调用——先补参数再调用。", name, desc)
	}
	return fmt.Sprintf("请补上 %s 参数（必填）。该参数为必填，"+
		"不要重复本次调用——先补参数再调用。", name)
}

// checkArgType 校验单个值的类型，**只拦真正无法解析的形态**。
//
// 放行清单（依据 utils.go 的宽松解析约定与实测的模型输出形态）：
//
//	· boolean：true/false、"true"/"false"/"1"/"0"/"yes"/"no"、0/1
//	· integer：int、int64、float64（整数值）、"20" 这类数字字符串
//	  （unitNumberRe 修的正是这种）、含单位字符串（"20s"）
//	· string：string；以及**结构体**（见下）
//	· array：[]interface{}、[]string
//	· object：map[string]interface{}
//
// ⚠️ string 放行结构体：模型常把复杂值塞进声明为 string 的参数
// （cmd 的 command 就常被写成含 JSON 的长文本）。拦它等于制造新失败；
// 真要用错时工具内部会自己报"格式不对"，那已足够。
func checkArgType(v interface{}, want string) *sdk.ToolError {
	if want == "" {
		return nil
	}
	// 显式 null 一律放行：模型有意传 null 时，工具侧按零值处理，
	// 拦它等于制造新失败（这正是本函数最该避免的）。
	if v == nil {
		return nil
	}
	switch want {
	case "string":
		// 宽松：只要不是显式的 bool/数字/数组/对象，基本都算字符串意图。
		// 只在**明显是容器/标量错配**时报错。
		switch v.(type) {
		case []interface{}, []string, map[string]interface{}:
			return newToolError(ErrReasonType, "", "", "")
		}
		return nil

	case "integer":
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return nil
		case float32, float64:
			return nil // JSON 解码的常态
		case string:
			// 数字或带单位字符串都放行（getFloat 会解析）。
			t := strings.TrimSpace(x)
			if t == "" {
				return nil
			}
			if _, err := strconv.ParseFloat(strings.TrimRight(t, "msdh"), 64); err == nil {
				return nil
			}
			// 非数字字符串：可能是 "20s" 这类带单位的（unitNumberRe 的目标形态）
			trimmed := strings.TrimRightFunc(t, func(r rune) bool {
				return r == 's' || r == 'm' || r == 'h' || r == 'd'
			})
			if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
				return nil
			}
			return newToolError(ErrReasonType, "",
				fmt.Sprintf("参数需要整数，收到 %q", x),
				"请改传数字（如 20 或 20.0），或把该参数改用 string 并带单位（如 \"20s\"）。")
		case bool:
			return newToolError(ErrReasonType, "",
				"参数需要整数，收到布尔值", "请改传数字。")
		default:
			return newToolError(ErrReasonType, "",
				fmt.Sprintf("参数需要整数，收到 %T", v), "请改传数字。")
		}

	case "number":
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64:
			return nil
		case string:
			if _, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return nil
			}
			return newToolError(ErrReasonType, "",
				fmt.Sprintf("参数需要数字，收到 %q", x),
				"请改传数字，或把该参数改用 string。")
		}
		return nil

	case "boolean":
		// getBool 的宽松形态全部放行（true/false/"1"/"0"/"yes"/... 与 0/1）。
		return nil

	case "array":
		switch v.(type) {
		case []interface{}, []string:
			return nil
		}
		return newToolError(ErrReasonType, "",
			fmt.Sprintf("参数需要数组，收到 %T", v), "请改传数组，如 [\"a\", \"b\"]。")

	case "object":
		switch v.(type) {
		case map[string]interface{}:
			return nil
		}
		return newToolError(ErrReasonType, "",
			fmt.Sprintf("参数需要对象，收到 %T", v), "请改传对象，如 {\"k\": \"v\"}。")
	}
	return nil
}

// validateArgsAgainstSchema 按工具声明的 schema 校验参数。
//
// 两条来源都要查：插件工具走 StageHost，设备/通道工具走 IOManager
// （cmd_run / files_write 都属后者——只查前者会让它们完全绕过校验）。
// **查不到 schema 就放行**：没有声明不等于参数非法。
func (a *Agent) validateArgsAgainstSchema(tc agentAPI.ToolCall) *sdk.ToolError {
	if a == nil {
		return nil
	}
	if a.stageHost != nil {
		if def := a.stageHost.ToolDef(tc.Name); def != nil {
			return validateToolArgs(tc.Arguments, def.Parameters)
		}
	}
	if a.io != nil {
		if def, ok := a.io.ToolDefOf(tc.Name); ok {
			return validateToolArgs(tc.Arguments, def.Parameters)
		}
	}
	return nil
}
