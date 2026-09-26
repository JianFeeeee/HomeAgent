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

## 五、接线前探明的六个**语义**（决定「C 化到什么程度」）

第二刀把库验完后，接线前又探了一轮 wire 语义。其中两条**直接推翻了
「整条 parseOpenAICompatibleStreamChunkFull 全 C 化」的设想**。

### 5.1 重复键是**字段级合并**（`json.Unmarshal` 的数组语义）

```
{"choices":[{"delta":{"content":"a"}}],"choices":[{"delta":{"reasoning_content":"r"}}]}
  → content="a"  reasoning="r"     ← 两个都保留！
{"choices":[{"delta":{"content":"a"}}],"choices":[{"delta":{}}]}
  → content="a"                    ← 第二次是空 delta，也没把 content 清掉
{"usage":{"prompt_tokens":1},"usage":{"completion_tokens":2}}
  → usage={1,2,0}                  ← 字段级合并
```

**机制**：`d.saveError(&d.array)` 保存目标；`object()` 收尾时执行
`v.SetIndex(i, subv.v)`，而 subv 解析时拿到的是**已存在元素的指针**
⇒ 第二次 unmarshal 是**叠加**在第一次之上的，不是替换。

⇒ 「第二个 element 整体覆盖第一个」是**错的**。正确实现需要维护
**「本次哪些字段出现过」的** 逐字段表**。这能做，但要显式建模。

### 5.2 `stringifyContent` 的默认分支 = `json.Marshal(interface{})`（**再编码**）

这是最关键的一条。`content` 是 `interface{}`，落到 default 分支时
**重新序列化一遍**：

| content 输入 | stringifyContent 输出 |
|---|---|
| `{"b":1,"a":2}` | `{"a":2,"b":1}`（**键排序**） |
| `{"k":"<a>&b"}` | `{"k":"\u003ca\u003e\u0026b"}`（**HTML 转义**） |
| `1e2` | `100`（float64 归一） |
| `1.0` | `1` |
| `123456789012345678` | `123456789012345680`（float64 舍入） |
| `1e21` | `1e+21` |

要让 C 版与 Go 逐值一致，就必须复刻 Go 的：
① 浮点**最短往返**格式化（`strconv.AppendFloat` 的 Ryu 语义，位数随值变化）
② `map` **按键排序**（Go 的 map 无序 ⇒ 排序是 Marshal 的确定性来源）
③ 字符串的 **HTML 转义 + ` / ` 转义**
④ int → **float64 舍入**再格式化

这不是「顺手写一下」的量级，而是一整套序列化器 + 一个浮点格式化器。

### 5.3 结论：C 化**降级**为「结构导航」层，序列化留在 Go

本条不是为了少做事，而是因为上面两条决定了一个可检验的事实：

> **C 负责把 JSON 定位到「哪个值在哪里」（零分配、零解码）；
> Go 负责把「已定位的原始字节」变成 `interface{}`（`json.Unmarshal`），
> 再按既有逻辑变成字符串。**

C 层因此**无需**理解重复键的合并语义（5.1）、**无需**实现浮点格式化
与键排序（5.2）—— 它只回答「`choices[0].delta.content` 的 span 在哪」。

代价与收益（如实记录）：

| | 收益 | 代价 |
|---|---|---|
| C 定位 | 免除 `json.Unmarshal` 的**反射建树**（每块 12~21 allocs 的主因） | 命中字段仍要一次小 `Unmarshal` |
| 保留 Go 序列化 | 5.1/5.2 的语义**逐字**保持，不需要两套实现 | 值转换仍有少量 alloc |

**唯一例外（已实测可达）**：`arguments` 若 upstream 发的是**非字符串**
对象/数组，Go 侧会 `json.Marshal` 重新编码（`{"b":2,"a":1}` → `{"a":1,"b":2}`），
**重新编码的键序可能与原文不同**。这类值必须走 Go（见接线实现的注释）。

### 5.4 其余四条语义（接线时直接照做即可）

| # | 语义 | 实测 |
|---|---|---|
| 1 | **key 大小写敏感**（map key） | `{"TEXT":"up"}` 取不到 `text`；但 `{"CHOICES":[{"DELTA":{"CONTENT":"ci"}}]}` 有效（struct 字段名不敏感） |
| 2 | `content` 数组：非对象元素**静默跳过** | `["a",{"text":"b"}]` → `"b"` |
| 3 | `content` 数组：`text` 非字符串**静默跳过** | `[{"text":123},{"text":"b"}]` → `"b"` |
| 4 | `index` 非整数 ⇒ **整块作废** | `{"index":1.5}` → false |
| 5 | usage 的 cache 字段类型错也**让整块作废** | `{"prompt_cache_hit_tokens":"x","prompt_tokens":1}` → false |

第 1 条与 ha_json_scan 的 `ha_json_key_eq`（大小写不敏感）**语义相反**，
两者用途不同、互不冲突（见 §5.3）—— 但必须在代码里注明，否则后人会「统一」掉。

## 六、接线实测：**本架构比原实现慢**（诚实记录，已默认关闭）

第三刀把 `ha_json_scan` 接进了生产路径（`chunkParseFast`），
**6 万+ 差分用例证明它与原实现逐值等价**（含语法、成员、解码、整数、
随机 JSON、随机字节五组）。但基准给出了**否定结论**，故**默认关闭**。

### 6.1 实测对比（`codec_chunkfast_bench_test.go`，20000 次迭代）

| 场景 | 新路径（Entry） | 原实现（GoOnly） | 结论 |
|---|---:|---:|---|
| content_zh | 2245 ns / 20 allocs | 2038 ns / 13 allocs | 更慢 |
| content_ascii | **2016 ns / 20 allocs** | **1325 ns / 13 allocs** | 慢 52% |
| toolcall | **5854 ns / 33 allocs** | **3270 ns / 21 allocs** | 慢 79% |
| usage | 3170 ns / 24 allocs | 2832 ns / 12 allocs | 更慢 |
| finish | 1560 ns / 20 allocs | 1098 ns / 12 allocs | 更慢 |

分配数**也变多**（20 vs 13）—— 与本刀「消除 GC 抖动」的初衷相反。

### 6.2 根因（逐项测出来的，不是猜的）

| 测量 | 数值 | 含义 |
|---|---:|---|
| 裸 cgo 调用（无 out-param） | **168 ns** | 一次性边界成本 |
| 带 out-param 的键查找 | **205 ns / 2 allocs** | 边界 + out-param 逃逸到堆 |
| 一次解析需要的键查找次数 | **5+** | choices→[0]→delta→content/reasoning/tool_calls→finish_reason |

⇒ **5 × 205ns ≈ 1µs 的边界与分配成本，恰好把收益全部吃掉。**
而 Go 侧是**一次** `json.Unmarshal` 遍历建整棵树。

**根因一句话**：本架构是「用很多次廉价调用，换一次昂贵调用」——
在这个尺寸上不划算。逐字段往返是设计错误，不是实现调优能救的。

### 6.3 天花板实验：方向对，但当前实现没到

为判断「还值不值得改」，我测了一个假设性上界 —— **假设拿到 span 完全免费**
（span 预先算好），只测本设计中**必须由 Go 做**的那部分：

| | ns/op | allocs |
|---|---:|---:|
| 我设计里的 Go 侧工作（零边界成本） | **505** | **7** |
| 原实现（整块 json.Unmarshal） | 1239 | 13 |

⇒ 若边界成本能压到近零，**仍有 2.4× 时间与 46% 分配的空间**。
故这不是「C 化没意义」，而是「**逐字段往返**这个交互方式是错的」。

### 6.4 正确的下一步（已由实测指明）

改造方向不是调优现有代码，而是**减少跨界次数**：

1. **一次 C 调用返回全部字段的 span**（批量），而不是逐字段往返
   —— 把 5+ 次边界压成 1 次
2. **结果写入调用方栈上的 C 结构体**，消除 out-param 逃逸（那 2 allocs）
3. 仅在 content/usage **确需重新编码**时回退 Go

### 6.5 为什么把「一个没有启用的优化」连代码一起提交

- **正确性基准**：6 万+ 差分用例已把 C 与 Go 的逐值等价钉死，
  这是改造的**已验证起点**（field-locating 与全部回退判据都验证正确了）
- **一条永不静默回退的机制**：`TestChunkFast_BenchGate` 断言
  `chunkFastEnabled` 必须为 `false`。后来者看到「快速路径写得挺全 +
  差分测试全过」，很自然会以为它已生效并打开它 —— 而实测它更慢。
  断言把这个事实钉住，改动即判红。
- **诚实**：不把「写了但没效果」包装成「已完成」。

> 教训（与第一刀同源）：**「C 比 Go 快」不是前提，是待验证的假设。**
> 第一刀推翻过一次（`C.CString` 造成 82% 自找开销），这一刀又推翻一次
> （逐字段往返造成 5+ 次边界）。两次都是**测量**推翻了直觉。

### 已知边界（诚实记录）

- `ha_json_get_int` 返回 `long long`；Go 侧 usage 字段是 `int`（64 位平台相同，
  32 位平台需截断检查）。当前未做平台相关处理 —— 内核只发布 linux/amd64 与
  linux/arm64（均 64 位），故暂不构成问题，但若将来上 32 位需补。
- 契约测试里 `check_str` 的重载写法偏笨拙（C 无重载），但已够用。

> ⚠️ 纪律：与第一刀同 —— **C 与纯 Go 逐值等价由黄金对照测试钉死**，
> 且**不做按长度分派**（两条语义可能分叉的实现绝不允许同时在产线）。
