//go:build cgo

package api

// codec_chunkfast_c.go —— parseOpenAICompatibleStreamChunkFull 的 C 快速路径
//
// ============================ 契约（务必先读） ============================
// 本文件是**纯优化**：它必须与 chunkParseGo 对**所有输入**产出完全相同的
// (StreamChunk, bool)。保证方式不是「小心写」，而是结构上的三条：
//
//  1. 任一环节判「不确定」⇒ **整体回退** chunkParseGo。没有任何分支
//     「尽力猜」或「部分采用」。
//  2. 每个「C 已判过合法」的子树，都用**与 Go 侧完全相同的 Go 类型**去
//     unmarshal ⇒ 类型检查语义天然一致，不靠 C 复刻类型规则。
//  3. 拼装（chunkAssemble）与工具调用归一化（normalizeStreamToolCall）
//     由两条路径**共用**，结构上无法分叉。
//
// 回退触发条件（穷举）：
//   · 顶层不是「恰好一个」良构对象（含尾部残留，见 ha_sse_root_object）
//   · 发现**重复键**（§5.1 字段级合并语义，C 不实现）
//   · content 是对象/数字/字面量（stringifyContent 需 json.Marshal 重新编码，§5.2）
//   · tool_calls 元素畸形 / 数组元素过多
//   · 任何子树畸形或缓冲不足
//
// 实测：真实负载三种块全部走快速路径；探针里的畸形、重复键、对象 content
// 等形态全部命中回退。
//
// ============================ ★ 当前默认**关闭**（实测比原实现慢） ============================
// 见 codec_chunkfast_bench_test.go 的实测：
//   content_ascii  Entry 2016ns/20allocs  vs  GoOnly 1325ns/13allocs
//   toolcall       Entry 5854ns/33allocs  vs  GoOnly 3270ns/21allocs
//   usage          Entry 3170ns/24allocs  vs  GoOnly 2832ns/12allocs
//
// 根因（已逐项测量，不是猜测）：
//  1. **每次键查找 205ns + 2 allocs**（out-params 逃逸到堆），
//     而**裸 cgo 边界就有 168ns**。一次解析需要 5+ 次查找
//     （choices→[0]→delta→content/reasoning/tool_calls→finish_reason）
//     ⇒ 边界成本 ≈ 1µs，恰好吃掉全部收益。
//  2. 每个字段还各自一次小 Unmarshal + 一次 decBuf 分配。
//  而 Go 侧是**一次** Unmarshal 遍历建整棵树。
//
// ⇒ 本架构是「**用很多次廉价调用换一次昂贵调用**」，在这个尺寸上不划算。
//   正确的前进方向是**减少边界次数**，而不是调优现有代码：
//     · 一次 C 调用返回**全部**字段的 span（批量，不逐字段往返）
//     · 结果写入**调用方栈上**的 C 结构体（消除 out-param 逃逸）
//     · 仅在 content/usage 确需重新编码时回退 Go
//   天花板实测：若边界成本归零，Go 侧代价 ≈ 505ns/7allocs
//   （对 1239ns/13allocs）⇒ **方向对，但当前实现没到**。
//
// ★ 保留本文件的理由：它同时是
//   ① 正确性基准（6 万+ 差分用例已钉死 C 与 Go 逐值等价）
//   ② 上述改造的**已验证起点**（field-locating 与回退判据都已验证正确）
//   ③ 一条**永不静默回退**的机制：若未来把它切回默认开启，
//      TestChunkFast_BenchGate 会立刻用基准把它按回去。


import "encoding/json"

const chunkFastEnabled = false

// chunkParseGo 是原始实现（整块 json.Unmarshal），作为快速路径的**唯一判据**
// 与回退目标。
func chunkParseGo(data string) (StreamChunk, bool) {
	var raw struct {
		Choices []struct {
			Delta struct {
				Content          interface{}      `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []openAIToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		UpstreamUsage chunkUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return StreamChunk{}, false
	}
	// 只把 choices[0] 转成装配用的形态 —— 与原实现一致（原实现只读 [0]，
	// 但 len() 判空用的是整个切片长度）。
	var choices []chunkChoice
	if len(raw.Choices) > 0 {
		c := raw.Choices[0]
		choices = []chunkChoice{{
			content:   stringifyContent(c.Delta.Content),
			reasoning: c.Delta.ReasoningContent,
			toolCalls: normalizeStreamToolCalls(c.Delta.ToolCalls),
			finishPtr: c.FinishReason,
		}}
	} else if len(raw.Choices) == 0 {
		choices = nil
	}
	return chunkAssemble(choices, raw.UpstreamUsage)
}

// chunkUsage 镜像 Go 侧的 UpstreamUsage 匿名结构。
type chunkUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	Prompt              int `json:"prompt"`
	Completion          int `json:"completion"`
	Total               int `json:"total"`
	PromptCacheHit      int `json:"prompt_cache_hit_tokens"`
	PromptCacheMiss     int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// chunkChoice 是装配用的形态：Content 已过 stringifyContent。
type chunkChoice struct {
	content   string
	reasoning string
	toolCalls []ToolCall
	// finishPtr 保留三态区分：缺失/null ⇒ nil；"" ⇒ 非 nil 但空串
	//（空串**不算**终止信号，sensenova 每块都发 ""）。
	finishPtr *string
}

// chunkAssemble 把已备好的选择与 usage 拼成 StreamChunk。
// **两条路径共用**它 ⇒ 拼装逻辑不可能分叉。
func chunkAssemble(choices []chunkChoice, usage chunkUsage) (StreamChunk, bool) {
	var u *TokenUsage
	if usage.Total > 0 || usage.TotalTokens > 0 ||
		usage.Prompt > 0 || usage.PromptTokens > 0 {
		u = &TokenUsage{
			Prompt:     pickFirstInt(usage.PromptTokens, usage.Prompt),
			Completion: pickFirstInt(usage.CompletionTokens, usage.Completion),
			Total:      pickFirstInt(usage.TotalTokens, usage.Total),
		}
	}
	if len(choices) == 0 {
		// 纯 usage 心跳块：有 usage 就透传，否则丢弃
		if u != nil {
			return StreamChunk{Usage: u}, true
		}
		return StreamChunk{}, false
	}
	c := choices[0]
	ck := StreamChunk{
		Content:          c.content,
		ReasoningContent: c.reasoning,
		ToolCalls:        c.toolCalls,
		Usage:            u,
	}
	if c.finishPtr != nil && *c.finishPtr != "" {
		ck.Done = true
		ck.FinishReason = *c.finishPtr
	}
	return ck, true
}

// ---------------------------------------------------------------------
// C 快速路径
// ---------------------------------------------------------------------

// chunkParseFast 尝试 C 快速路径。
// 返回 (chunk, handled, decided)：
//   handled=false            ⇒ 调用方必须用 chunkParseGo
//   handled=true,decided=true ⇒ 结果是最终答案
func chunkParseFast(data string) (StreamChunk, bool, bool) {
	root := rootSpan(data)
	if !sseRootObject(root) {
		return StreamChunk{}, false, false
	}

	// ---- usage：整棵子树交给 encoding/json ----
	// ★ 为什么逐个整数取是错的：Go 侧 usage 有 9 个字段，且**任一类型不符
	//   就让整块作废**（实测 {"prompt_cache_hit_tokens":"x","prompt_tokens":1}
	//   → 整块 false）。整棵 unmarshal 到**同一个 Go 类型** ⇒ 语义自动一致。
	var usage chunkUsage
	us, ufound, udup, ubad := findKeyCI(root, "usage")
	if ubad || udup {
		return StreamChunk{}, false, false
	}
	if ufound {
		switch us.firstByte() {
		case 'n':
			// null ⇒ 零值 struct（不产出 usage）
		case '{':
			if err := json.Unmarshal(us.bytes(), &usage); err != nil {
				// ★ 类型不符 ⇒ 与 Go 一样「整块作废」，**不需要回退**
				return StreamChunk{}, false, true
			}
		default:
			return StreamChunk{}, false, false // 交回 Go 决定
		}
	}

	// ---- choices：只取 [0]，但要先判整切片的长度语义 ----
	cs, cfound, cdup, cbad := findKeyCI(root, "choices")
	if cbad || cdup {
		return StreamChunk{}, false, false
	}
	if !cfound {
		ck, ok := chunkAssemble(nil, usage)
		return ck, true, ok
	}
	switch cs.firstByte() {
	case 'n':
		// null ⇒ 零值切片（长度 0）⇒ 走「无 choices」分支
		ck, ok := chunkAssemble(nil, usage)
		return ck, true, ok
	case '[':
	default:
		return StreamChunk{}, false, false
	}
	el, has := firstElem(cs)
	if !has {
		// 空数组：len(choices)==0 ⇒ 与 Go 相同
		ck, ok := chunkAssemble(nil, usage)
		return ck, true, ok
	}
	ch, ok := fastChoice(el)
	if !ok {
		return StreamChunk{}, false, false // 任何不确定 ⇒ 整体回退
	}
	ck, ok2 := chunkAssemble([]chunkChoice{ch}, usage)
	return ck, true, ok2
}

// fastChoice 解析 choices[0]。ok=false ⇒ 必须回退 Go。
//
// ★ 键匹配方式按 Go 那一跳的实际类型选择：
//   - choices / delta / finish_reason / tool_calls 是 **struct 字段** ⇒ 大小写不敏感
//   - content / reasoning_content / "text" 是 **interface{} → map key** ⇒ 大小写敏感
//   （实测：{"CHOICES":[{"DELTA":{"CONTENT":"ci"}}]} 有效；
//     {"content":[{"TEXT":"up"}]} 取不到 text）
func fastChoice(el strSpan) (chunkChoice, bool) {
	var ch chunkChoice
	if el.firstByte() != '{' {
		return ch, false
	}

	ds, dfound, ddup, dbad := findKeyCI(el, "delta")
	if dbad || ddup {
		return ch, false
	}
	if dfound {
		switch ds.firstByte() {
		case 'n':
			// delta:null ⇒ 零值 struct
		case '{':
			// ★ delta 是 **struct**（不是 map！）——
			//   原实现：Delta struct { Content interface{}; ... } `json:"delta"`
			//   故它的字段名匹配是**大小写不敏感**。
			//   实测 `{"CHOICES":[{"DELTA":{"CONTENT":"ci"}}]}` → content="ci"。
			//   只有 content 的**值**（若为对象/数组）才成为 map/[]interface{}，
			//   那时里面的键（如 "text"）才是大小写敏感。
			//
			//   我一度把这里改成 CS 并认为「差分测试会通过」——那是错的推理：
			//   Go 侧给的是 "ci"（CI 匹配成功），改成 CS 反而把快速路径弄丢。
			//   教训：**「哪一层是 struct、哪一层是 map」要回原实现读类型，
			//   不能凭字段名像 map 就推断它是 map。**

			// reasoning_content：Go 侧是 **string**（强类型）。
			// 用同样的 Go 类型 unmarshal ⇒ 123 会报错，与原实现一致。
			rs, rfound, rdup, rbad := findKeyCI(ds, "reasoning_content")
			if rbad || rdup {
				return ch, false
			}
			if rfound && rs.firstByte() != 'n' {
				var s string
				if err := json.Unmarshal(rs.bytes(), &s); err != nil {
					return ch, false // 类型不符 ⇒ 回退（Go 会整块作废）
				}
				ch.reasoning = s
			}

			// content 字段名：CI（struct 字段）。
			// 其**值**若是数组/对象，内部键由 ha_sse_stringify 按 CS 处理。
			cs, cfound, cdup, cbad := findKeyCI(ds, "content")
			if cbad || cdup {
				return ch, false
			}
			if cfound {
				if s, handled := stringifyC(cs); handled {
					ch.content = s
				} else {
					return ch, false // 需 json.Marshal 重新编码（§5.2）
				}
			}

			// tool_calls：逐个元素整体 unmarshal 成 openAIToolCall，
			// 使 arguments 的 interface{} 形态 / 类型检查全由 encoding/json 负责。
			tcs, tfound, tdup, tbad := findKeyCI(ds, "tool_calls")
			if tbad || tdup {
				return ch, false
			}
			if tfound {
				switch tcs.firstByte() {
				case 'n':
					// null ⇒ 零值切片
				case '[':
					t, ok := fastToolCalls(tcs)
					if !ok {
						return ch, false
					}
					ch.toolCalls = t
				default:
					return ch, false
				}
			}
		default:
			return ch, false
		}
	}

	// finish_reason：struct 字段 ⇒ 大小写不敏感；Go 侧是 *string
	fs, ffound, fdup, fbad := findKeyCI(el, "finish_reason")
	if fbad || fdup {
		return ch, false
	}
	if ffound && fs.firstByte() != 'n' {
		if fs.firstByte() != '"' {
			return ch, false
		}
		var s string
		if err := json.Unmarshal(fs.bytes(), &s); err != nil {
			return ch, false
		}
		ch.finishPtr = &s
	}
	return ch, true
}

// fastToolCalls 解析 tool_calls 数组。
//
// ★ 逐元素整体 unmarshal 成 openAIToolCall 是刻意的：这样 arguments 的
//   interface{} 形态、字符串/对象/数组/数字各分支、重复键，全部由
//   encoding/json 处理（§5.2 的重新编码语义不必在 C 复刻）。
//   归一化也走**同一个** normalizeStreamToolCall ⇒ 与 Go 路径不分叉。
func fastToolCalls(arr strSpan) ([]ToolCall, bool) {
	elems, ok := scanArray(arr)
	if !ok {
		return nil, false
	}
	if len(elems) == 0 {
		return nil, true // 空数组 ⇒ nil（与 Go 的 normalizeStreamToolCalls 一致）
	}
	out := make([]ToolCall, 0, len(elems))
	for _, elem := range elems {
		if elem.firstByte() != '{' {
			return nil, false
		}
		var raw openAIToolCall
		if err := json.Unmarshal(elem.bytes(), &raw); err != nil {
			return nil, false
		}
		out = append(out, normalizeStreamToolCall(raw))
	}
	return out, true
}
