#ifndef HA_JSON_SCAN_H
#define HA_JSON_SCAN_H

/*
 * ha_json_scan — HomeAgent 内核 LLM 协议层的 JSON 扫描/取值层（C 实现）
 *
 * ============================ 定位 ============================
 * 本库**不是**通用 JSON 库，是**流式协议分块解析**专用的零分配扫描层。
 * 它服务 `parseOpenAICompatibleStreamChunkFull`（每个 SSE chunk 跑一次的最热路径）。
 *
 * ★ 为什么不复用 SDK 的 remotedevice/src/ha_json.c（实测，见 plan.md §七）：
 *   1. 无 `\u` 解码 —— `\u4f60\u597d` 得到 `?0?d?d?0`（LLM 内容全靠转义时直接损坏）
 *   2. 只有 `_get_int`，无浮点 —— `temperature:0.7` **静默**变 0
 *   3. `null` 与「键缺失」不可区分
 *   4. 架构是 **DOM + malloc**，与「不 malloc / 零拷贝 / 纯函数」正交
 *   它的定位是 remotedevice 设备通道，不是 LLM 协议层。
 *
 * ============================ 设计：scan / extract 两段分离 ============================
 * **scan** 只出结构 span（键 span / 值 span），零分配、零解码、零求值。
 * **extract** 按 span 取值，解码只发生在真正需要它的调用方身上。
 *
 * 为什么必须分离：`content` 可能是很大的多模态数组，而 `stringifyContent`
 * 只需要「把 text 字段拼起来」。若 scan 阶段就为每个字符串 `\u` 解码并分配
 * 缓冲，等于把解码成本付给了不需要它的调用方 —— 那正是我们要消灭的分配。
 *
 * ============================ 不可协商的约束（与 ha_codec.h 同标准） ============================
 *   1. 只吃 `const char*` + **显式长度**，不要求 NUL 结尾
 *      （否则又是 `strlen` + 拷贝的老问题，见第一刀 §7.1 的 82% 自找开销）
 *   2. **不 malloc**：结果一律以 span（指针+长度）回给调用方，Go 侧零拷贝切片
 *   3. 无状态、纯函数、线程安全（不写全局可变状态）
 *   4. 语法语义必须与 Go `encoding/json` **一致**，由黄金对照测试钉死
 *
 * ============================ 语义对齐（易踩，全部实测） ============================
 *   - 键匹配**大小写不敏感**（Go `encoding/json` 行为）
 *   - 字符串取值时非法 UTF-8 每字节替换为 U+FFFD（与 Go 一致）
 *   - 重复键**后者胜**
 *   - 本层**不做类型检查**：`{"a":{}}` 对 `a` 的扫描成功，是否「类型不对应报错」
 *     由调用方按 Go 的 interface{} / 强类型语义决定（见 §三.2.2 的实测）
 */

#include <stddef.h>

#include "ha_abi.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ==================== ABI 版本（与 ha_abi.h 同步） ==================== */

#define HA_JSON_SCAN_ABI_MAJOR 1
#define HA_JSON_SCAN_ABI_MINOR 0
#define HA_JSON_SCAN_ABI_VERSION \
    (HA_JSON_SCAN_ABI_MAJOR * 1000 + HA_JSON_SCAN_ABI_MINOR)

HA_STATIC_ASSERT(HA_JSON_SCAN_ABI_MAJOR >= 1 && HA_JSON_SCAN_ABI_MAJOR <= 9,
                 ha_jsonscan_abi_major_in_range);
HA_STATIC_ASSERT(HA_JSON_SCAN_ABI_MINOR >= 0 && HA_JSON_SCAN_ABI_MINOR <= 99,
                 ha_jsonscan_abi_minor_in_range);

/* 返回 HA_JSON_SCAN_ABI_VERSION（供 Go 侧与日志核对）。 */
int ha_json_scan_abi_version(void);

/* ==================== span 与扫描器 ==================== */

/* 字节区间 [p, p+len)。指针指向**调用方的原缓冲**，本库从不持有或释放。 */
typedef struct {
    const char *p;
    size_t      len;
} ha_span;

/* 扫描器：对一段 JSON 文本的只读游标。
 *
 * ★ 就地结构体（非指针）：调用方在栈上持有，零分配。
 *   但因此**不可拷贝后混用**（拷贝出的副本与原游标各自独立推进）。
 */
typedef struct {
    const char *s;   /* 缓冲区起点 */
    size_t      n;   /* 缓冲区长度 */
    size_t      i;   /* 当前游标偏移 */
} ha_json_scan;

/* 用 (s, n) 初始化扫描器，游标置于起点。s 可为 NULL（此时按 n=0 处理）。 */
void ha_json_scan_init(ha_json_scan *sc, const char *s, size_t n);

/* 跳过前导 ASCII 空白（空格 / \t / \n / \r）。返回是否已到结尾。 */
int ha_json_scan_ws(ha_json_scan *sc);

/* 当前是否已到结尾（不含空白跳过）。 */
int ha_json_scan_eof(const ha_json_scan *sc);

/* 跳过**一个完整的 JSON 值**（对象 / 数组 / 字符串 / 数字 / 字面量）。
 *
 * 用于二次进数组内部（如 stringifyContent 取数组元素的 text 字段）：
 * 先 skip 前面的元素，再对目标元素单独扫描。
 * 返回 0 表示语法错误，1 表示成功。成功后游标停在该值之后。
 */
int ha_json_skip(ha_json_scan *sc);

/* 解析一个字符串值，出**原始字节 span**（含转义序列，未解码）。
 *
 * 入参：游标应停在 `"` 上（或之前的空白，函数自己跳过空白）。
 * 出参 raw：不含两端引号的原始内容 span（指向原缓冲，零拷贝）。
 * 返回 0 = 语法错误（未闭合 / 非字符串）。
 *
 * ★ 注意：不做 `\u` 解码、不做非法 UTF-8 替换 —— 那是 extract 阶段的事。
 */
int ha_json_scan_string(ha_json_scan *sc, ha_span *raw);

/* ==================== 顶层对象：扫描出键值对 ==================== */

/*
 * 顶层对象的迭代器。
 *
 * ★ 为什么由本库来切「顶层逗号」而不是让 C 侧只解析第一个键：
 *   LLM 的 `content` 里常含 `{`、`}`、`,`（代码、JSON 片段、模板）。
 *   若调用方自己按逗号切开顶层，会被内容里的逗号错切。
 *   本库扫**字符串感知**的边界，保证只在真正的顶层分隔符处切分。
 */
typedef struct {
    ha_json_scan sc;      /* 游标 */
    int          started;  /* 是否已消费过至少一个成员 */
    int          done;     /* 迭代是否已结束（正常或异常） */
    int          error;    /* 结束原因：1 = 输入畸形（而非正常的 '}'） */
} ha_json_members;

/* 初始化顶层对象迭代。非法（首个非空白字符不是 '{'）时返回 0。 */
int ha_json_members_init(ha_json_members *m, const char *s, size_t n);

/* 取下一个成员。
 *
 * 出参：
 *   key —— 键的原始字节 span（未解码，不含引号）；可为 NULL
 *   val —— 值的**完整 span**（未解码）；可为 NULL
 *
 * 返回： 1 = 拿到一个完整成员；0 = 结束。
 *
 * ★ 本函数**内部会完整跳过一个值**，因此：
 *   1. 返回 1 蕴含「这个成员是良构的」（值能独立被 skip）——
 *      调用方拿到的 val 一定可解析，不必自己再验一次。
 *   2. 游标在返回前已推进到值之后，下一次调用直接看下一个成员。
 *      （早期版本只报值的**起始位置**、不消费值，迫使调用方自己
 *       修正游标 —— 那是个错误的设计：调用方一旦忘了推进，下一个
 *       成员就会解析到上一个值，而模糊测试立刻把它暴露了出来。）
 *
 * ★ 结束时要区分原因：用 ha_json_members_complete() 判断是否正常。
 *   返回 0 既可能是「正常扫到 '}'」也可能是「输入畸形」——
 *   要复刻 Go 的严格性（畸形 ⇒ 整块作废）就必须能分辨。
 *
 * ★ 键匹配请用 ha_json_key_eq（大小写不敏感），不要自己 memcmp。
 */
int ha_json_members_next(ha_json_members *m, ha_span *key, ha_span *val);

/* 迭代是否**正常结束**（消费到闭合的 '}'）。
 *
 * 语义：只有在 next() 返回 0 之后才有意义。
 * 返回 1 = 对象良构且已完整扫描；0 = 输入畸形（缺 '}' / 尾逗号 /
 * 值非法等）。调用方若要复刻 Go 的严格性，应要求它为 1。
 */
int ha_json_members_complete(const ha_json_members *m);

/* 大小写不敏感地比较键 span 与 ASCII 字面量。返回 1/0。
 *
 * ★ 必须用它而不是 memcmp：Go `encoding/json` 的键匹配**大小写不敏感**，
 *   实测 `{"delta":{"CONTENT":"up"}}` 能取出 content="up"。
 *   逐字节比对会静默漏掉这类输入。 */
int ha_json_key_eq(ha_span key, const char *name);

/* ==================== 取值（extract） ==================== */

/* 字符串解码的**写入回调**。
 *
 * ★ 为什么用回调而不是「分配缓冲返回」：本库不 malloc。调用方把自己的
 *   Go 侧 buffer / 栈缓冲 / 直接写目标的位置交给本库，解码结果逐个 rune
 *   以 UTF-8 字节写入 —— 非法序列按 Go 语义替换为 U+FFFD。
 *
 * ★ 为什么按 rune 而不是按字节：`\uXXXX` 可能产生多字节 rune（含代理对
 *   合成的 4 字节 emoji），调用方不该关心编码细节。
 */
typedef void (*ha_json_sink)(void *ctx, const char *utf8_bytes, size_t len);

/* 把字符串值 span（raw 形式，含转义）解码并经 sink 输出。
 *
 * 出参 out_len：解码后的字节总数（便于调用方预分配 / 校验）。
 * 返回 0 = 原始 span 含**语法错误**（如 \u 后不是 4 位十六进制）。
 *
 * 非法 UTF-8 处理：与 Go `encoding/json` 一致 —— 每个非法字节一个 U+FFFD
 * （**不是**按整个序列丢弃）。见真值表 §2.8。
 */
int ha_json_decode_string(ha_span raw, ha_json_sink sink, void *ctx,
                          size_t *out_len);

/* 把字符串值 span 解码进调用方提供的缓冲（不足则失败，不截断）。
 *
 * 返回写入的字节数；缓冲不足时返回 (size_t)-1 且不写。
 * 适合长度已知且不关心「只需长度」的场景。
 */
size_t ha_json_decode_string_into(ha_span raw, char *out, size_t out_cap);

/* 读整数（仅接受 JSON 整数语法，可选负号；不允许小数点/指数）。
 *
 * ★ 与 Go 的对应关系：Go 里 `int` 字段会接受 `1e2`（=100）与拒绝 `1.5`；
 *   本函数**只认纯整数**，指数/小数由调用方按「类型不匹配 ⇒ 整块作废」
 *   语义处理（见真值表 §2.2）。这样职责清晰：本层只回答「这是不是整数」。
 *
 * 返回 1 = 成功且 *out 已写；0 = 不是合法整数。
 * 溢出返回 0（与 Go 报错等价）。
 */
int ha_json_get_int(ha_span raw, long long *out);

/* ==================== 字符串取值的便捷路径 ==================== */

/* 在对象 span 内取键 name 的字符串值，解码进 out（NUL 结尾）。
 *
 * 返回：解码后字节数（不含结尾 NUL）；键缺失 / 类型不是字符串 / 缓冲不足
 * 返回 (size_t)-1。out 在成功时保证 NUL 结尾。
 *
 * 便捷函数：内部走 members 迭代 + key_eq + decode，适合调用方只取一两个键
 * 且不需要「类型不匹配 ⇒ 整块作废」细节的场景。
 */
size_t ha_json_object_get_string(ha_span obj, const char *name,
                                 char *out, size_t out_cap);

/* 在对象 span 内取键 name 的整数值。
 * 返回 1 = 成功；0 = 键缺失 / 非合法整数 / 溢出。 */
int ha_json_object_get_int(ha_span obj, const char *name, long long *out);

#ifdef __cplusplus
}
#endif

#endif /* HA_JSON_SCAN_H */
