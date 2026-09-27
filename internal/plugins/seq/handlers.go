package seq

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// 本文件是 seq_* 工具的实现：解析参数 → 调 Store / 执行引擎 → 渲染结果。
//
// 两条贯穿全文件的纪律：
//  1. **不静默降级**：参数缺失、序列不存在、目标不存在都**报错**并说明
//     该怎么做。模型拿到含糊的"失败"只会原样重试。
//  2. **错误信息要可执行**：说清缺什么、可用的是什么。

// seqCreate 创建/更新序列。groups 与 file 二选一。
func (p *Plugin) seqCreate(args map[string]interface{}) (interface{}, error) {
	name := argString(args, "name")
	if name == "" {
		return nil, fmt.Errorf("缺少 name（序列名）")
	}
	groupsRaw, hasGroups := args["groups"]
	file := argString(args, "file")

	switch {
	case file != "" && hasGroups:
		return nil, fmt.Errorf("groups 与 file **二选一**，不能同时传")
	case file == "" && !hasGroups:
		return nil, fmt.Errorf("必须提供 groups 或 file 其中之一（长序列建议写文件后用 file 传）")
	}

	var (
		text []byte
		from = "参数"
	)
	if file != "" {
		b, err := p.readSeqFile(file)
		if err != nil {
			return nil, err
		}
		text = b
		from = "文件 " + file
	} else {
		b, err := marshalGroups(groupsRaw)
		if err != nil {
			return nil, err
		}
		text = b
	}

	seq, err := Parse(text)
	if err != nil {
		return nil, fmt.Errorf("来自%s的序列解析失败: %w", from, err)
	}
	if seq.Name == "" {
		seq.Name = name
	}
	if seq.Name != name {
		return nil, fmt.Errorf("序列名不一致：参数给了 %q，内容里是 %q（请统一）", name, seq.Name)
	}
	if d := argString(args, "description"); d != "" {
		seq.Description = d
	}
	if err := p.store.Save(seq); err != nil {
		return nil, err
	}
	// 跨序列引用可能成环：存完复查一次（Save 只校验同序列内的 group 引用）
	graphErr := p.store.CheckGraph()

	var sb strings.Builder
	fmt.Fprintf(&sb, "序列 %q 已保存（%s，%d 个 group）", seq.Name, from, len(seq.Groups))
	for _, g := range seq.Groups {
		fmt.Fprintf(&sb, "\n- %s", g.Name)
		if len(g.In) > 0 {
			fmt.Fprintf(&sb, " 入参[%s]", keyList(g.In))
		}
		if len(g.Out) > 0 {
			fmt.Fprintf(&sb, " 出参[%s]", keyList(g.Out))
		}
		fmt.Fprintf(&sb, " 工具%d个", len(g.Tools))
	}
	if graphErr != nil {
		// 保存成功但图不合法 —— 必须**说清**，否则模型会以为可以跑了
		fmt.Fprintf(&sb, "\n⚠️ 序列已保存，但调用图有问题（现在执行会失败）：%v", graphErr)
	}
	return sb.String(), nil
}

// readSeqFile 读序列文件，带路径逃逸防护。
func (p *Plugin) readSeqFile(path string) ([]byte, error) {
	abs, err := absPath(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("序列文件 %s 不存在", path)
		}
		return nil, fmt.Errorf("读序列文件 %s 失败: %w", path, err)
	}
	return b, nil
}

// absPath 做基础路径校验：拒绝空、拒绝明显的穿越写法。
func absPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("路径为空")
	}
	if strings.Contains(p, "\x00") {
		return "", fmt.Errorf("路径含非法字符")
	}
	// 允许绝对与相对路径，但禁止 .. 段（与 files 插件的目录逃逸防护同源思路）
	for _, seg := range strings.Split(filepathToSlash(p), "/") {
		if seg == ".." {
			return "", fmt.Errorf("路径 %q 含 .. 段，不允许", p)
		}
	}
	return p, nil
}

func filepathToSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// marshalGroups 把 groups 参数（[]interface{}）序列化为 JSON 文本。
func marshalGroups(raw interface{}) ([]byte, error) {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("groups 必须是数组，实际是 %T", raw)
	}
	doc := map[string]interface{}{"groups": arr}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("groups 序列化失败（每个 group 应为对象）: %w", err)
	}
	return b, nil
}

// seqList 列出全部序列及其签名。
func (p *Plugin) seqList() (interface{}, error) {
	names := p.store.List()
	if len(names) == 0 {
		return "当前没有任何序列（用 seq_create 新建）", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "共 %d 条序列:", len(names))
	for _, n := range names {
		seq, err := p.store.Load(n)
		if err != nil {
			fmt.Fprintf(&sb, "\n- %s ⚠️ 读取失败: %v", n, err)
			continue
		}
		desc := seq.Description
		if desc == "" {
			desc = "（无描述）"
		}
		fmt.Fprintf(&sb, "\n- %s：%s（%d 个 group）", n, desc, len(seq.Groups))
		for _, g := range seq.Groups {
			fmt.Fprintf(&sb, "\n    · %s", g.Name)
			if len(g.In) > 0 {
				fmt.Fprintf(&sb, " 入参[%s]", keyList(g.In))
			}
			if len(g.Out) > 0 {
				fmt.Fprintf(&sb, " 出参[%s]", keyList(g.Out))
			}
		}
	}
	return sb.String(), nil
}

// seqDelete 删除序列。
func (p *Plugin) seqDelete(args map[string]interface{}) (interface{}, error) {
	name := argString(args, "name")
	if name == "" {
		return nil, fmt.Errorf("缺少 name（序列名）")
	}
	if err := p.store.Delete(name); err != nil {
		return nil, err
	}
	return fmt.Sprintf("序列 %q 已删除", name), nil
}

// seqRun 执行一条序列的全部 group。
func (p *Plugin) seqRun(args map[string]interface{}) (interface{}, error) {
	name := argString(args, "name")
	if name == "" {
		return nil, fmt.Errorf("缺少 name（序列名）")
	}
	seq, err := p.store.Load(name)
	if err != nil {
		return nil, err
	}
	in, _ := args["args"].(map[string]interface{})
	if in == nil {
		in = map[string]interface{}{}
	}
	return p.runSequence(seq, in, nil)
}

// seqCall 按名调用一个 group（when 非空时为条件调用）。
func (p *Plugin) seqCall(args map[string]interface{}, when string) (interface{}, error) {
	target := argString(args, "target")
	if target == "" {
		return nil, fmt.Errorf("缺少 target（组名或 #序列名）")
	}
	seqName := argString(args, "name")
	callArgs, _ := args["args"].(map[string]interface{})
	if callArgs == nil {
		callArgs = map[string]interface{}{}
	}

	var seq *Sequence
	var err error
	if strings.HasPrefix(target, "#") {
		seqName = strings.TrimPrefix(target, "#")
		seq, err = p.store.Load(seqName)
	} else {
		if seqName == "" {
			return nil, fmt.Errorf("按组名调用时必须给出 name（该组所属的序列名）")
		}
		seq, err = p.store.Load(seqName)
	}
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, g := range seq.Groups {
		if g.Name == target || strings.TrimPrefix(target, "#") == g.Name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("序列 %q 里没有 group %q（现有：%s）",
			seq.Name, target, groupNameList(seq))
	}

	// 条件调用：求值在**进入前**，为假则整次跳过
	if strings.TrimSpace(when) != "" {
		ok, cerr := evalCond(when, callArgs)
		if cerr != nil {
			// ★ 求值失败**不得**降级为"跳过"
			return nil, fmt.Errorf("seq_when_call 的条件求值失败（这是参数问题，不是工具故障）: %w", cerr)
		}
		if !ok {
			return fmt.Sprintf("条件为假，已跳过 %q（不产出任何槽）", target), nil
		}
	}

	res, gerr := p.runGroup(seq.Groups[idx], callArgs, seq.Name)
	if gerr != nil {
		return nil, gerr
	}
	return renderGroupResult(res), nil
}

// runSequence 顺序执行全部 group。
//
// group 间**串行**：后者可能依赖前者的出参槽（具名槽即数据边）。
func (p *Plugin) runSequence(seq *Sequence, in map[string]interface{}, _ any) (interface{}, error) {
	if p.callDepth >= maxCallDepth {
		return nil, fmt.Errorf("嵌套调用深度超过上界 %d（可能存在循环调用）", maxCallDepth)
	}
	p.callDepth++
	defer func() { p.callDepth-- }()

	var lines []string
	slots := map[string]interface{}{}
	failed := false

	for i, g := range seq.Groups {
		// 每组的入参 = 顶层入参 + 已产出槽（具名槽在组间传递）
		groupArgs := map[string]interface{}{}
		for k, v := range in {
			groupArgs[k] = v
		}
		for k, v := range slots {
			groupArgs[k] = v
		}
		res, err := p.runGroup(g, groupArgs, seq.Name)
		if err != nil {
			lines = append(lines, fmt.Sprintf("第 %d 组 %q 失败: %v", i+1, g.Name, err))
			failed = true
			if g.OnError != "continue" {
				break
			}
			continue
		}
		line := fmt.Sprintf("第 %d 组 %q", i+1, g.Name)
		if res.Skipped {
			line += "（条件为假，已跳过）"
		} else {
			line += fmt.Sprintf(" 工具 %d 个", len(res.Tools))
			if len(res.Missing) > 0 {
				line += fmt.Sprintf(" ⚠️缺失工具: %s", strings.Join(res.Missing, ", "))
			}
		}
		lines = append(lines, line)
		// 槽合并（后续组可读）
		for k, v := range res.Slots {
			slots[k] = v
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "序列 %q 执行完毕（%d/%d 组）:", seq.Name, len(lines), len(seq.Groups))
	for _, l := range lines {
		sb.WriteString("\n- " + l)
	}
	if len(slots) > 0 {
		sb.WriteString("\n\n变量槽:")
		keys := make([]string, 0, len(slots))
		for k := range slots {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "\n  %s = %s", k, truncate(renderResult(slots[k]), 160))
		}
	}
	if failed {
		sb.WriteString("\n⚠️ 有 group 失败（见上）")
	}
	return sb.String(), nil
}

// runGroup 执行单个 group，含黑名单、并发安全与深度检查。
func (p *Plugin) runGroup(g Group, args map[string]interface{}, seqName string) (GroupResult, error) {
	// ① 黑名单先行
	for _, t := range g.Tools {
		if blacklisted(t.Tool) {
			return GroupResult{Group: g.Name}, fmt.Errorf(
				"group %q 试图调用被禁止的工具 %s"+
					"（序列不得对外发消息/改插件表/再起子 agent）", g.Name, t.Tool)
		}
		if t.Tool == "seq_call" || t.Tool == "seq_when_call" {
			tgt, _ := t.Args["target"].(string)
			if strings.HasPrefix(tgt, "#") {
				if strings.TrimPrefix(tgt, "#") == seqName {
					return GroupResult{Group: g.Name}, fmt.Errorf("group %q 调用了序列自身（无限递归）", g.Name)
				}
			}
		}
	}

	// ② 组内是否真的可以并发：全部声明 ParallelSafe 才并发。
	//    查不到声明 ⇒ 保守按串行（动态注册下工具可能随时消失）。
	runnable := g.Parallel
	if runnable {
		for _, t := range g.Tools {
			if !p.runner.parallelSafe(t.Tool) {
				runnable = false
				break
			}
		}
	}
	if !runnable {
		g.Parallel = false
	}

	// ③ 存在性预检（missing 策略的输入）
	preMissing := false
	for _, t := range g.Tools {
		if t.Tool == "seq_call" || t.Tool == "seq_when_call" {
			continue // 内建调用另行处理
		}
		if !p.runner.exists(t.Tool) {
			preMissing = true
			break
		}
	}
	if preMissing {
		switch g.missingPolicy() {
		case "skip", "degrade":
			// 逐个剔除缺失的工具
			var kept []ToolCall
			for _, t := range g.Tools {
				if (t.Tool == "seq_call" || t.Tool == "seq_when_call") || p.runner.exists(t.Tool) {
					kept = append(kept, t)
				}
			}
			if len(kept) == 0 {
				return GroupResult{Group: g.Name, Skipped: true, Slots: map[string]interface{}{}}, nil
			}
			g.Tools = kept
		}
	}

	return execGroup(g, args, p.runner)
}

func groupNameList(seq *Sequence) string {
	names := make([]string, 0, len(seq.Groups))
	for _, g := range seq.Groups {
		names = append(names, g.Name)
	}
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, ", ")
}

func renderGroupResult(res GroupResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "group %q", res.Group)
	if res.Skipped {
		sb.WriteString("（条件为假，已跳过）")
		return sb.String()
	}
	fmt.Fprintf(&sb, " 完成 %d 个工具", len(res.Tools))
	if len(res.Missing) > 0 {
		fmt.Fprintf(&sb, "，缺失: %s", strings.Join(res.Missing, ", "))
	}
	if len(res.Slots) > 0 {
		keys := make([]string, 0, len(res.Slots))
		for k := range res.Slots {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString("\n出参:")
		for _, k := range keys {
			fmt.Fprintf(&sb, "\n  %s = %s", k, truncate(renderResult(res.Slots[k]), 160))
		}
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
