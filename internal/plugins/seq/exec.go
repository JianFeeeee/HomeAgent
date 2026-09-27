package seq

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// toolRunner 是执行引擎对「执行一个工具」的依赖。
//
// ⚠️ 它**必须**接收 args：插值是本阶段的核心能力之一，若接口不给出实际
// 收到的参数，插值就无法被任何判据观察（我第一版写成 call(name) 时，
// 插值判据就成了摆设——它永远"通过"）。
type toolRunner interface {
	call(name string, args map[string]interface{}) (string, error)
}

// errToolNotFound 表示「工具不存在」（未注册 / 插件未加载、已卸载或崩溃）。
//
// 它是**本包定义**的标记，不复用内核的 agentIO.ErrToolNotFound：seq 是插件，
// 拿得到的是 sdk.ToolAPI（ExecuteTool/GetAllTools），拿不到 io 包的类型
// （见设计文档 §7 的边界声明）。内核侧的类型化错误本就要经 D4 才下放到插件。
var errToolNotFound = errors.New("工具不存在或未注册")

// IsToolNotFound 报告 err 是否为「工具不存在」。
func IsToolNotFound(err error) bool { return errors.Is(err, errToolNotFound) }

// GroupResult 是一组的执行结果。
type GroupResult struct {
	Group   string
	Skipped bool // 条件为假而整组跳过
	Slots   map[string]interface{}
	Tools   []ToolRun
	// Missing 列出因「工具不存在」而被 skip/degrade 的工具名。
	Missing []string
	Err     error
}

// ToolRun 是组内单个工具的执行记录。
type ToolRun struct {
	Name   string
	Result string
	Err    error
	Order  int // 在 tools 数组中的位置（合并按它，不按完成顺序）
}

// execGroup 执行一组工具。
//
// 三个不变量（各自有判据钉住）：
//  1. **组内并行、组间串行**：parallel=true 时各工具并发跑。
//  2. **合并按声明顺序**：结果写入各自槽位的"暂存区"，
//     组屏障处按 tools 数组顺序**一次性**合并。
//     并行下完成顺序不确定；若按完成顺序合并，同样的输入会产出不同的
//     序列输出 —— 整条流程不可复现。
//     顺序合并同时解决了并发写 map 的问题：**执行期不写共享 map**。
//  3. **条件求值失败必须报错**，不得降级成"条件为假"。
func execGroup(g Group, args map[string]interface{}, runner toolRunner) (GroupResult, error) {
	res := GroupResult{Group: g.Name, Slots: map[string]interface{}{}}

	// ① 条件求值（在任何执行之前）
	ok, err := evalCond(g.When, args)
	if err != nil {
		return res, fmt.Errorf("group %q 的条件求值失败: %w", g.Name, err)
	}
	if !ok {
		res.Skipped = true
		return res, nil // 槽**不赋值**：后续组引用它时由静态校验/调用方发现
	}

	n := len(g.Tools)
	results := make([]ToolRun, n)

	run := func(i int) {
		tc := g.Tools[i]
		// 插值：把 $args.* 换成实参。整值引用保留原始类型。
		realArgs := substituteArgs(tc.Args, args)
		out, err := runner.call(tc.Tool, realArgs)
		results[i] = ToolRun{Name: tc.Tool, Result: out, Err: err, Order: i}
	}

	// ② 执行
	if g.Parallel && n > 1 {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				run(idx)
			}(i)
		}
		wg.Wait()
	} else {
		for i := 0; i < n; i++ {
			run(i)
		}
	}

	// ③ 按**声明顺序**合并槽位（不按完成顺序 —— 见函数注释）
	failed := false
	var firstErr error
	for _, r := range results {
		res.Tools = append(res.Tools, r)
		tc := g.Tools[r.Order]

		// 「工具不存在」单独处理：动态注册下它是**常态**（插件未加载/崩溃），
		// 与「执行失败」语义不同 —— 前者该按 missing 策略走，后者才该 retry。
		//
		// ⚠️ 必须先于通用的 on_error 检查：若「不存在」先被记成 firstErr/
		//   failed，missing skip/degrade 就会被 on_error=abort 连坐中断。
		if r.Err != nil && IsToolNotFound(r.Err) {
			dealMissing(g, tc, r, &res, &failed, &firstErr)
			continue
		}

		if r.Err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("工具 %s 失败: %w", r.Name, r.Err)
			}
			if g.OnError != "continue" {
				failed = true
			}
		}

		if tc.As == "" {
			continue
		}
		// 失败时也留槽（记错误文本），否则后续组读到的是"缺失"，
		// 而"缺失"与"值为空"在下游难以区分。
		val := interface{}(r.Result)
		if r.Err != nil {
			val = "错误：" + r.Err.Error()
		}
		if isArrayType(g.Out[tc.As]) {
			cur, _ := res.Slots[tc.As].([]interface{})
			res.Slots[tc.As] = append(cur, val)
		} else {
			res.Slots[tc.As] = val
		}
	}

	if failed {
		res.Err = firstErr
		return res, firstErr
	}
	return res, nil
}

// substituteArgs 把 args 里的 $args.* 替换为实参值。
//
// 两种替换形态（缺一不可）：
//
//	· **整值引用**："$args.count" 整个值就是引用 ⇒ 替换为**原始值**，
//	  保留类型（数字仍是数字）。否则模型会收到字符串 "3" 而非数字。
//	· **文本内插值**："ssh $args.host" ⇒ 在字符串内替换。
func substituteArgs(args map[string]interface{}, scope map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		out[k] = substituteValue(v, scope)
	}
	return out
}

func substituteValue(v interface{}, scope map[string]interface{}) interface{} {
	switch t := v.(type) {
	case string:
		// 整值引用
		if key, ok := wholeRef(t); ok {
			if val, present := scope[key]; present {
				return val // 保留原始类型
			}
			return t // 未提供则原样保留（静态校验已保证它被声明过）
		}
		// 文本内插值
		return interpolate(t, scope)
	case map[string]interface{}:
		return substituteArgs(t, scope)
	case []interface{}:
		arr := make([]interface{}, len(t))
		for i, e := range t {
			arr[i] = substituteValue(e, scope)
		}
		return arr
	}
	return v
}

// wholeRef 判断字符串是否**整体**是一个 $args.<键> 引用。
func wholeRef(s string) (string, bool) {
	refs := parseArgRefs(s)
	if len(refs) == 1 {
		trimmed := strings.TrimSpace(s)
		if strings.HasPrefix(trimmed, "$args."+refs[0]) &&
			strings.TrimSuffix(trimmed, "$args."+refs[0]) == "" {
			return refs[0], true
		}
	}
	return "", false
}

// interpolate 在文本内把 $args.<键> 替换为实参的**字符串形式**。
func interpolate(s string, scope map[string]interface{}) string {
	refs := parseArgRefs(s)
	if len(refs) == 0 {
		return s
	}
	var sb strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "$args.")
		if i < 0 {
			sb.WriteString(rest)
			return sb.String()
		}
		sb.WriteString(rest[:i])
		rest = rest[i+len("$args."):]
		end := 0
		for end < len(rest) && isRefChar(rest[end]) {
			end++
		}
		if end == 0 {
			continue
		}
		key := rest[:end]
		rest = rest[end:]
		if val, present := scope[key]; present {
			sb.WriteString(scalarToString(val))
		} else {
			sb.WriteString("$args." + key)
		}
	}
}

// scalarToString 渲染标量值。
//
// ⚠️ 对象/数组用**紧凑 JSON**，绝不用 fmt.Sprintf("%v")——那会产出
// `map[k:v]` 这种模型读不懂的 Go 语法（core 的 renderToolResult 同样约定）。
func scalarToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	default:
		return compactJSON(t)
	}
}

// evalCond 求值 `when` 条件。
//
// 支持（L1+L2，见设计文档 §5）：
//   - `true` / `false`
//   - `$args.key`（布尔真值）
//   - `$args.key` 存在性与非空判断：!= "" 、== ""
//   - 比较：== / != / > / < / >= / <=（标量）
//   - `contains`：文本包含
//
// ⚠️ **求值出错必须返回 error**，不得当作 false。
// 把失败降级成"跳过"= 序列安静地少做一步，而模型以为跑完了 ——
// 与「静默吞工具」同族。
func evalCond(cond string, args map[string]interface{}) (bool, error) {
	c := strings.TrimSpace(cond)
	if c == "" || c == "true" {
		return true, nil
	}
	if c == "false" {
		return false, nil
	}

	// 形如 "$args.flag == true" / "$args.n > 3" / "$args.s contains x"
	if left, op, right, ok := splitComparison(c); ok {
		lv, err := resolveOperand(left, args)
		if err != nil {
			return false, err
		}
		rv, err := resolveOperand(right, args)
		if err != nil {
			return false, err
		}
		return compare(lv, op, rv)
	}

	// 裸引用：真值判断
	if key, ok := wholeRef(c); ok {
		v, present := args[key]
		if !present {
			return false, fmt.Errorf("引用了未提供的入参 $args.%s", key)
		}
		return truthy(v), nil
	}

	return false, fmt.Errorf("无法解析的条件表达式 %q"+
		"（支持：true/false、$args.x、$args.x == 值、$args.x contains \"子串\"、大小比较）", cond)
}

// splitComparison 拆出 `左 op 右`（op 需含空格，避免与 contains 前缀混淆）。
func splitComparison(c string) (left, op, right string, ok bool) {
	ops := []string{" contains ", " >= ", " <= ", " == ", " != ", " > ", " < "}
	for _, o := range ops {
		if i := strings.Index(c, o); i >= 0 {
			return strings.TrimSpace(c[:i]), strings.TrimSpace(o), strings.TrimSpace(c[i+len(o):]), true
		}
	}
	return "", "", "", false
}

// resolveOperand 解析操作数：字面量或 $args 引用。
func resolveOperand(s string, args map[string]interface{}) (interface{}, error) {
	s = strings.TrimSpace(s)
	if key, ok := wholeRef(s); ok {
		v, present := args[key]
		if !present {
			return nil, fmt.Errorf("引用了未提供的入参 $args.%s", key)
		}
		return v, nil
	}
	// 字面量
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1], nil
	}
	switch strings.ToLower(s) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, nil
	}
	return s, nil // 裸文本
}

// compare 按运算符比较两个标量。
func compare(l interface{}, op string, r interface{}) (bool, error) {
	if op == "contains" {
		ls, lok := l.(string)
		rs, rok := r.(string)
		if lok && rok {
			return strings.Contains(ls, rs), nil
		}
		// 非字符串：退化为字符串化包含
		return strings.Contains(scalarToString(l), scalarToString(r)), nil
	}
	if op == "==" || op == "!=" {
		eq := scalarToString(l) == scalarToString(r)
		// 布尔与字符串宽松比较：true == "true"
		if !eq {
			eq = strings.EqualFold(scalarToString(l), scalarToString(r))
		}
		if op == "==" {
			return eq, nil
		}
		return !eq, nil
	}
	// 大小比较：两侧都必须是数值
	lf, lok := toFloat(l)
	rf, rok := toFloat(r)
	if !lok || !rok {
		return false, fmt.Errorf("运算符 %q 两侧必须是数值（得到 %T 与 %T）", op, l, r)
	}
	switch op {
	case ">":
		return lf > rf, nil
	case "<":
		return lf < rf, nil
	case ">=":
		return lf >= rf, nil
	case "<=":
		return lf <= rf, nil
	}
	return false, fmt.Errorf("未知运算符 %q", op)
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// truthy 标量真值：非零数字、true、非空文本、非空集合。
func truthy(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case []interface{}:
		return len(t) > 0
	case map[string]interface{}:
		return len(t) > 0
	}
	return true
}

// groupTimeout 返回本组的墙钟上限（Group.Timeout 形如 "30s"）。
func groupTimeout(g Group) time.Duration {
	if g.Timeout == "" {
		return 0 // 由调用方决定默认
	}
	d, err := time.ParseDuration(g.Timeout)
	if err != nil {
		return 0
	}
	return d
}

// compactJSON 渲染结构化值为紧凑 JSON。
//
// ⚠️ 绝不用 fmt.Sprintf("%v")：那会产出 `map[k:v]` 这种模型读不懂的
// Go 语法（core 的 renderToolResult 同样约定，见设计文档 §5.2）。
func compactJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// parseFallback 解析 degrade 的兜底值（紧凑 JSON 文本）。
// 解析失败时原样作为字符串返回——兜底值本身不该让整组失败。
func parseFallback(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

// dealMissing 处理「工具不存在」这一**常态**情形（动态注册下插件可能
// 未加载、已卸载或崩溃），按 group 的 missing 策略处置。
//
// 与「执行失败」严格分开：后者才该走 on_error / retry。若把两者混同，
// 一条"插件挂了"会被当成业务失败反复重试，或反过来该重试的被整组跳过。
func dealMissing(g Group, tc ToolCall, r ToolRun, res *GroupResult, failed *bool, firstErr *error) {
	switch g.missingPolicy() {
	case "skip":
		res.Missing = append(res.Missing, tc.Tool)
		return // 不给槽赋值（与「条件为假」同一情形：下游要能应对槽缺失）
	case "degrade":
		res.Missing = append(res.Missing, tc.Tool)
		if tc.As != "" {
			res.Slots[tc.As] = parseFallback(tc.Fallback)
		}
		return
	default: // fail
		*failed = true
		if *firstErr == nil {
			*firstErr = fmt.Errorf("工具 %s 不存在或未注册"+
				"（可能属于未加载/已崩溃的插件；用 seq_list 看可用序列，或改用其他工具）", tc.Tool)
		}
		if tc.As != "" {
			res.Slots[tc.As] = "错误：工具不存在"
		}
	}
}
