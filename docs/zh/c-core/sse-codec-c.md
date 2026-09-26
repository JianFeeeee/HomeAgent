# 内核 C 化 · 第二刀：SSE 分块协议编解码

> 分支：`feature/c-core`（承接第一刀，见 `llm-orchestration-c.md`）
> 状态：**扫描/取值库已落地并闭环**（2026-09-26）。
> 基础设施已建成（`plan.md` §七），本文记录第二刀的**判据**、
> **Go 侧真值表**、**不可协商的约束**与**落地记录**。
>
> 本刀范围（有意收窄）：**只交付 `ha_json_scan` 库 + 与 Go 的逐值对照**。
> **尚未**改动 Go 生产路径（`parseOpenAICompatibleStreamChunkFull` 仍是原实现）——
> 接线是独立一步，需单独验证与基准，避免「库还没验就换产线」。

---

## 一、为什么是 SSE 分块编解码

判据不是「哪个看起来底层」，而是「**在真实负载下值不值**」。

`parseOpenAICompatibleStreamChunkFull` 是**每个流式 chunk 都要跑一次**的最热路径。
实测（`feature/c-core`，153 字节 content 块）：

| 输入 | ns/op | allocs/op |
|---|---:|---:|
| content 块（含中文） | 1937 | 13 |
| toolcall 块 | **3122** | **21** |
| usage 块 | 2464 | 12 |
| 纯字节扫描理论下限 | **133** | 1 |

差距 **15–23×**。一次 1 万块的会话 = 1–2 万次堆分配 —— 这正是 C 化的原始动机
（消除 GC 抖动）。

> 注：真值表探针（`zz_truth_test.go`，临时）测出 `usage` 块 2464ns/12 allocs，
> 而上面表格里 153 字节的 content 块是 1937ns/13 allocs。两者接近，但**探针的
> usage 输入 248 字节更大**，说明这张表只看数量级，具体值随输入形状浮动。

---

## 二、★ Go 侧真值表（本刀的**规格**）

C 实现不是「重新设计」，是**逐值复刻 Go**。而 Go 的 `encoding/json` 语义里
藏着一批**不直观的行为**——先探明再写 C，否则会造出一个「看起来对」的错实现。

以下全部为实测（`go test -run TestGroundTruth`）：

### 2.1 键匹配是**大小写不敏感**的

```
{"choices":[{"DELTA":{"CONTENT":"up"}}]}     → 解析成功，content="up"
{"choices":[{"delta":{"content":"x"},"FINISH_REASON":"stop"}]} → done=true
```

★ 极易踩：手写解析器若逐字节比对键名，这两种输入会**静默返回空内容**。
必须走「键长度 + 大小写不敏感比较」。

### 2.2 类型不匹配 ⇒ **整块作废**（不是「该字段降级为空」）

```
{"choices":[{"delta":{"content":{}}}]}          → FALSE（整块拒绝）
{"choices":[{"delta":{"reasoning_content":123}}]} → FALSE
{"usage":{"prompt_tokens":"1"}}                  → FALSE
{"usage":{"prompt_tokens":1.5}}                  → FALSE
{"usage":{"prompt_tokens":1e2}}                  → FALSE
{"usage":{"prompt_tokens":99999999999999999999}}→ FALSE（溢出 ⇒ 报错）
{"choices":[{"delta":{"content":"x"},"finish_reason":42}]} → FALSE
```

★ 这是本刀**最反直觉**的一条：Go 侧「某字段类型不对」**不是**忽略该字段，
而是让 `json.Unmarshal` 整体失败、`parseOpenAICompatibleStreamChunkFull` 返回 `false`，
于是该 chunk 被 `continue` 静默丢弃。

⇒ 后果：上游若发来一个 usage 心跳块（只有 `prompt_cache_hit_tokens`、
`prompt_tokens_details`，没有 `prompt_tokens`/`total_tokens`/`prompt`），
**整块被丢弃**。实测确认：
```
{"usage":{"prompt_cache_hit_tokens":5,"prompt_tokens_details":{"cached_tokens":7}}} → FALSE
```
这在语义上「无害」（那个块本来也只有缓存细节），但它说明一件事：
**C 侧若比 Go 宽松，会让本来被丢的块开始生效，token 统计口径就变了**。

### 2.3 `content` 是 `interface{}`，走 `stringifyContent`

| 输入类型 | 结果 |
|---|---|
| `"hi"` | `"hi"` |
| `null` | `""` |
| `123` | `"123"` |
| `[{"type":"text","text":"a"}]` | `"a"`（数组取每个对象的 `text` 拼接） |
| `{}` | **整块 FALSE**（默认分支 `json.Marshal` 后 unmarshal 失败） |

### 2.4 `reasoning_content` 是**强类型 string**

`123` ⇒ 整块 FALSE（与 `content` 的宽松形成对比）。空 `delta` 正常通过。

### 2.5 语法严格性

`{` / `{"a":}` / ``（空）/ `null` / `[]` / `"str"` / `123` / `{"a":1,}`（尾逗号）/
`{'a':1}`（单引号）**全部 FALSE**。

★ 顶层非对象必须 FALSE（`json.Unmarshal` 到 struct 会报
`cannot unmarshal array into Go value of type struct`）。

### 2.6 重复键：**后者胜**（与 `ha_json.c` 相同）

```
{"choices":[{...content:"a"}],"choices":[{...content:"b"}]} → content="b"
{"usage":{"total_tokens":1},"usage":{"total_tokens":2}}     → total=2
```

### 2.7 `finish_reason` 语义

| 值 | 结果 |
|---|---|
| `null` | `Done=false`（指针为 nil） |
| `"stop"` | `Done=true`, `FinishReason="stop"` |
| `""` | `Done=false`（**空串不算终止信号**，注释说明是 sensenova 每块都发 `""`） |
| 缺失 | `Done=false` |
| `42` | 整块 FALSE |

### 2.8 非法 UTF-8：Go 侧替换为 U+FFFD

```
{"content":"\xff\xfe"} → content="\uFFFD\uFFFD"（两个替换字符）
```

⇒ C 侧的 `\uXXXX` 与字符串取值必须与 Go 的替换语义一致
（`utf8.RuneError` 编码为 `EF BF BD`，**一个非法字节 = 一个 U+FFFD**，
不是按序列整体丢弃）。

### 2.9 转义

`\" \\ \/ \n` 等正常解码；`你好😀` 直接 UTF-8 透传。

---

## 三、不可协商的约束

1. **只吃 `(const char*, size_t)`**，不要求 NUL 结尾（否则又是 `strlen` + 拷贝的
   老问题，见第一刀 §7.1 的 82% 自找开销教训）
2. **不 malloc**：结果用 **span（指针+长度）** 回给调用方，Go 侧零拷贝切片
3. **无状态纯函数、线程安全**
4. **顶层**：`ha_json_scan_chunks` 出 (span × N, 浅扫，含字符串内的 `{`/`}`，
   让**顶层逗号分隔**可被切分——这正是协议层的需要：内容里的逗号不该错切顶层）
5. **语法**必须与 `encoding/json` 一致（含尾逗号非法、`null`/标量顶层非法、
   重复键后者胜、大小写不敏感键匹配）

### 设计：scan(结构) + extract(取值) 两段分离

理由：`content` 可能是一个**很大**的多模态数组；而 `stringifyContent` 只需要
「text 字段拼起来」。若 scan 阶段就为每个字符串做 `\u` 解码并分配缓冲，
就等于把「解码」付给了不需要它的调用方。

故：
- **scan**：只出结构 span（键 span / 值 span）。零分配、零解码。
  还要能**二次进数组内部**（取 `text` 字段）——故 API 需 `ha_json_skip`。
- **extract**：按 span 取值。字符串解码 (\u + 非法字节替换)、整数、布尔分别独立函数。

---

## 四、落地记录（2026-09-26）

| 项 | 状态 | 证据 |
|---|---|---|
| Go 侧真值表 | ✅ | 本文 §二（含三处「纠正自己的错表」） |
| `ha_json_scan.{c,h}` | ✅ | 零分配、span 返回、scan/extract 两段分离 |
| C 契约测试 | ✅ | `test_ha_json_scan.c`：**119 项断言全过** |
| 黄金对照（逐值比对 Go） | ✅ | `codec_jsongolden_test.go`：语法/成员/解码/整数/随机字节 5 组全过 |
| 模糊测试 | ✅ | `test_fuzz_ha_json_scan.c`：**4948 万次运行零崩溃** |
| 接入 Go 生产路径 | ⏳ | **有意未做**：库先验完再换产线，接线是独立一步 |

### ★ 本刀被测试抓出的真实缺陷（7 个，全部记入代码注释防复发）

写 C 时**同一份逻辑我读了三遍都认为正确**，是测试把它们逐个揪出来的。
这正是「黄金对照 + 模糊测试」不可省的理由 —— 手写解析器的错不是崩溃，
而是**静默分叉**（少一个字符、某些块被丢弃），生产里极难归因。

| # | 缺陷 | 症状 | 谁抓到 |
|---|---|---|---|
| 1 | 代理对合成成功后**未跳过** unconditionally 的 U+FFFD 发射 | `\ud83d\ude00`（😀）→ 两个 U+FFFD | 契约测试 |
| 2 | 过长编码检查用了**只含首字节位**的 cp | `你`(e4 bd a0) → 6 个 U+FFFD | 契约测试 |
| 3 | `members_next` 只报值起点、**不消费值** | 游标停在值前 → 下个成员解析到上一个值 | **模糊测试第一轮** |
| 4 | 扫描阶段**不校验**转义字符合法性 | `{"a":"\q"}` C 判合法、`json.Valid`=false | 黄金对照 |
| 5 | 扫描阶段**不校验** `\u` 后四位十六进制 | `{"a":"\u00"}` 同上 | 黄金对照 |
| 6 | `get_int` **接受前导零** | `007`/`00` C 认、JSON 非法 | 黄金对照 |
| 7 | cgo 桥接把 C 结构体声明为 Go 局部变量 | `cgo argument has Go pointer to unpinned Go pointer` panic | Go 运行时 |

**另外纠正了我自己两次错误的「真值」**（比代码 bug 更危险，因为它会变成错误的规格）：
- 第一版真值表里 `content:{}` 的花括号**少了一层**，于是把「我写错了 JSON」
  误读成「Go 对 content 类型严格」。修正后实测：`content:{}` → `"{}"`（**宽松**）。
- 由此才看出一对**方向相反**的语义：`content` 走 `interface{}` **宽松**
  （`{}`→`"{}"`、`true`→`"true"`、`1.5`→`"1.5"`），而 `reasoning_content` /
  `usage` / `finish_reason` 是**强类型严格**（`123` ⇒ 整块作废）。
  若照错误的表去写 C，会产出一个「比 Go 更严格」的实现，静默丢弃本该生效的块。

### 两个设计决定（来自缺陷 3、7）

1. **`members_next` 返回完整值 span 并内部跳过它**
   —— 让「返回 1」蕴含「该成员良构」。要求调用方自己推进游标的 API 是错的：
   忘一次就会解析到上一个值（缺陷 3），而这种错**不会报错**。
2. **`members_complete()` 区分「正常扫到 `}`」与「输入畸形」**
   —— 复刻 Go 的严格性必须能分辨二者，否则畸形输入会被当正常结束。

### 刻意保留的能力（当前调用方用不到，但设计上不该省）

`\uXXXX` 解码（含代理对合成）。本次内核的 Go 基线里没有这种输入（实测确认），
但**上游网关的行为不由我们控制** —— 日志与已拦缺陷记录显示，网关确会发
`content` 为 JSON 字符串的形态。留着它是防止未来某条上游路径切到转义形态时，
内容**静默变成 `?0?d?d?0`**（那正是 SDK `ha_json.c` 的缺陷 1 的形态）。
代价是约 10 行代码 + 一组已通过的测试。

### 已知边界（诚实记录）

- `ha_json_get_int` 返回 `long long`；Go 侧 usage 字段是 `int`（64 位平台相同，
  32 位平台需截断检查）。当前未做平台相关处理 —— 内核只发布 linux/amd64 与
  linux/arm64（均 64 位），故暂不构成问题，但若将来上 32 位需补。
- 契约测试里 `check_str` 的重载写法偏笨拙（C 无重载），但已够用。

> ⚠️ 纪律：与第一刀同 —— **C 与纯 Go 逐值等价由黄金对照测试钉死**，
> 且**不做按长度分派**（两条语义可能分叉的实现绝不允许同时在产线）。
