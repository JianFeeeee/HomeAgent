#ifndef HA_SSE_H
#define HA_SSE_H

/*
 * ha_sse — LLM 流式协议（SSE 分块）的「结构导航」辅助层
 *
 * ============================ 定位 ============================
 * 本层不是 JSON 库（那是 ha_json_scan），而是把 ha_json_scan 原语组合成
 * **协议层需要的几次定位**，供内核 parseOpenAICompatibleStreamChunkFull 使用。
 *
 * ★ 为什么这些函数放在 csrc/ 而不是内联在 Go 的 cgo 前言里：
 *   放在 cgo 前言里的 C 代码**逃出了全部 C 门禁**（告警 / ASan+UBSan /
 *   交叉编译 / 模糊测试），而它恰恰是本刀最容易出错的位置。
 *   移进 csrc/ 后，同一个 -Wall -Wextra -Wpedantic -Wconversion 门禁
 *   与 sanitizer 都覆盖到它 —— 这是一次真实的结构调整，不是形式主义。
 *
 * ============================ 为什么只做「导航」 ============================
 * 实测两条 wire 语义（docs/zh/c-core/sse-codec-c.md §5）使「全量 C 化」不成立：
 *   §5.1 重复键是**字段级合并**（json.Unmarshal 的 SetIndex 叠加语义）
 *   §5.2 stringifyContent 的 default 分支是 json.Marshal（键排序 / 浮点
 *        最短往返 / HTML 转义 / int 舍入）
 * 二者都只在**取值**阶段需要，故本层只回答「值在哪里、它的热分支结果是什么」，
 * 需要重新序列化的形态交回 Go（由 encoding/json 保证语义）。
 *
 * ============================ 键匹配：大小写敏感 ============================
 * 本层是 **map key** 语义（`m["text"]`）⇒ 大小写敏感。
 * 实测 `{"TEXT":"up"}` 取不到 `text`、`{"text":"low"}` 可以（§5.4-1）。
 *
 * ⚠️ 与 ha_json_key_eq（大小写**不**敏感，用于 struct 字段名）语义相反。
 *    两者用途不同、都必要，**不要「统一」掉**。
 *    struct 字段那一跳由 encoding/json 负责，天然正确。
 */

#include <stddef.h>

#include "ha_abi.h"
#include "ha_json_scan.h"

#ifdef __cplusplus
extern "C" {
#endif

#define HA_SSE_ABI_MAJOR 1
#define HA_SSE_ABI_MINOR 1
#define HA_SSE_ABI_VERSION (HA_SSE_ABI_MAJOR * 1000 + HA_SSE_ABI_MINOR)

HA_STATIC_ASSERT(HA_SSE_ABI_MAJOR >= 1 && HA_SSE_ABI_MAJOR <= 9,
                 ha_sse_abi_major_in_range);
HA_STATIC_ASSERT(HA_SSE_ABI_MINOR >= 0 && HA_SSE_ABI_MINOR <= 99,
                 ha_sse_abi_minor_in_range);

int ha_sse_abi_version(void);

/*
 * 在对象里按**大小写敏感**的键定位值。
 *
 * 返回： 1 = 找到（*out 已写）；0 = 未找到（对象良构）；-1 = 对象畸形。
 * *dup 在发现**重复键**时置 1（后者胜已写入 *out）——
 *   调用方据此整体回退到 encoding/json，因为重复键的字段级合并语义
 *   见 sse-codec-c.md §5.1，本层不实现。
 */
int ha_sse_obj_find(const ha_span *obj, const char *key, size_t keylen,
                    ha_span *out, int *dup);

/*
 * 在对象里按**大小写不敏感**的键定位值（struct 字段语义）。
 *
 * ★ 为什么必须与 ha_sse_obj_find 并存（两个函数，语义相反）：
 *   - Go 的 `raw struct{ Choices ... \`json:"choices"\` }` 是 **struct 字段**，
 *     encoding/json 对字段名做**大小写不敏感**匹配 ⇒ 实测
 *     `{"CHOICES":[{"DELTA":{"CONTENT":"ci"}}]}` 能取到 content="ci"。
 *   - 而 `content` 是 `interface{}` → `map[string]interface{}`，取 `m["text"]`
 *     是 **map key** 语义 ⇒ 大小写**敏感**（实测 `{"TEXT":"up"}` 取不到）。
 *   跳错层就会静默漏掉字段（或取到不该取的），故两个函数都必要，
 *   调用方必须按「这一跳在 Go 里是 struct 还是 map」来选择。
 *
 * 返回与 ha_sse_obj_find 相同：1=找到 0=未找到 -1=畸形。
 * 对 *dup：大小写不敏感语义下，`{"CHOICES":..,"choices":..}` 两次都会命中
 * 同一个 Go 字段（后者胜），故同样置 dup 让调用方回退。
 */
int ha_sse_obj_find_ci(const ha_span *obj, const char *key, size_t keylen,
                       ha_span *out, int *dup);

/*
 * 校验 doc 是「**恰好一个**良构 JSON 对象」（尾部只允许空白）。
 *
 * 返回 1 = 是；0 = 否。
 *
 * ★ 为什么必须单独校验尾部：ha_sse_obj_find 用 members_complete 只保证
 *   对象本身闭合，**不检查尾部残留** —— 而 Go 的 json.Unmarshal 会拒绝
 *   `{"a":1}{"b":2}`（trailing garbage）。少了这一步，快速路径会比 Go 宽松，
 *   把一个 Go 判为失败的块判为成功 ⇒ 静默接受垃圾块。
 */
int ha_sse_root_object(const ha_span *doc);

/*
 * 取数组**第一个元素**的 span。
 *
 * 返回： 1 = 有元素；0 = 空数组；-1 = 非数组或畸形。
 *
 * 为什么只要第一个：`choices[0]` 是协议约定（Go 侧也只读 resp.Choices[0]），
 * 本层据此避免为后续元素做无用功。
 */
int ha_sse_arr_first(const ha_span *arr, ha_span *out);

/*
 * stringifyContent 的 **C 可判定分支**：
 *   - 字符串值  → 反转义后原样输出
 *   - 数组值    → 逐元素取对象的 "text" 字段（精确键）拼接
 *
 * 返回： 1 = 已写入（*outlen 为字节数）；0 = 需回退 Go。
 *   回退的两种情形：
 *     a) 缓冲不足（调用方应给 >= val->len*3+4 的 cap）
 *     b) 值类型是对象 / 数字 / 字面量 —— 那些要走 json.Marshal（§5.2）
 *
 * 数组元素的规则（§5.4-2/3 实测）：
 *   · 非对象元素 **静默跳过**（`["a",{"text":"b"}]` → "b"）
 *   · 非对象的 "text"（如 text:123）**静默跳过**
 *   · 元素里出现重复的 "text" 键 ⇒ 整体回退 Go（合并语义）
 */
int ha_sse_stringify(const ha_span *val, char *out, size_t cap, size_t *outlen);

/*
 * arguments 为**字符串**时，取出其解码结果（省掉 interface{} 与二次解析）。
 *
 * 返回 1 = 已写入；0 = 不是字符串或失败（调用方按既有路径处理）。
 * 非字符串 arguments（对象/数组/数字）**必须**回退 Go：那里的
 * `rawArgsString` 会 `json.Marshal` 重新编码，而这个**重新编码的键序
 * 可能与原文不同**（实测 {"b":2,"a":1} → {"a":1,"b":2}）——
 * 逐值一致要求由 encoding/json 来做。
 */
int ha_sse_arg_string(const ha_span *val, char *out, size_t cap, size_t *outlen);

/* ==================================================================== */
/* 批量定位：**一次调用**返回整块解析所需的全部字段                     */
/* ==================================================================== */
/*
 * ★ 为什么需要它（第三刀实测的教训，见 sse-codec-c.md §六）：
 *   逐字段往返做 5+ 次 cgo 调用，每次约 168ns 边界 + 2 allocs（out-param
 *   逃逸到堆）⇒ 约 1µs 固定成本，把全部收益吃光，结果比原实现更慢。
 *
 *   本接口把它压成 **1 次调用**，并顺带解决另外两点：
 *     · **单趟键分派**：不再「每个键各扫一遍对象」，而是遍历一次成员表
 *       就分派（原来 6 次扫描 → 2 次）
 *     · **解码内联**：content / reasoning_content 的解码在同一趟里写进
 *       调用方缓冲，不再各来一次往返
 *
 * 结果写在调用方的 ha_chunk_out 里（C 结构体、无 Go 指针 ⇒ 可安全传指针）。
 */

/* 槽位索引（固定约定，**改动必须 bump ABI**）。 */
#define HA_CHUNK_SLOT_DELTA         0
#define HA_CHUNK_SLOT_CONTENT       1
#define HA_CHUNK_SLOT_REASONING     2
#define HA_CHUNK_SLOT_TOOL_CALLS    3
#define HA_CHUNK_SLOT_FINISH_REASON 4
#define HA_CHUNK_SLOT_USAGE         5
#define HA_CHUNK_SLOT_COUNT         6

/* 槽位类型。与 Go 侧「该字段是什么 Go 类型」对应，而非单纯 JSON 类型。 */
#define HA_CHUNK_KIND_ABSENT 0
#define HA_CHUNK_KIND_NULL   1
#define HA_CHUNK_KIND_STRING 2
#define HA_CHUNK_KIND_OBJECT 3
#define HA_CHUNK_KIND_ARRAY  4
#define HA_CHUNK_KIND_OTHER  5  /* 数字 / 布尔 */

/* ha_sse_chunk_locate 返回码。 */
#define HA_CHUNK_OK          0  /* 定位成功，可用快速路径 */
#define HA_CHUNK_FALLBACK   -1  /* 需回退 Go：重复键 / 畸形 / 顶层非对象 /
                                 * 多 choices / 缓冲不足 */
#define HA_CHUNK_TYPE_FAIL  -2  /* 与 Go 一致的「整块作废」（类型不符） */

typedef struct {
    ha_span span;  /* 原始值 span（未解码，指向 data） */
    int     kind;  /* HA_CHUNK_KIND_*  */
} ha_chunk_slot;

typedef struct {
    ha_chunk_slot slot[HA_CHUNK_SLOT_COUNT];
    /* choices 数组本身的 span（choices_count>0 时有效） */
    ha_span choices_span;
    /* choices[0] 的 span（choices_count==1 时有效） */
    ha_span choice0_span;
    /* 解码/反转义结果（写入 sbuf，以 [off,len) 表示；kind 非字符串时为 (0,0)） */
    size_t content_off;   size_t content_len;
    size_t reasoning_off; size_t reasoning_len;
    size_t finish_off;    size_t finish_len;

    int has_choices;    /* choices 是否存在且非 null */
    int choices_kind;   /* ABSENT / NULL / ARRAY */
    int choices_count;  /* 元素个数（>1 时调用方必须回退，见下） */
    int choice0_kind;   /* ABSENT / NULL / OBJECT */
} ha_chunk_out;

/*
 * 一次调用定位整块解析所需的全部字段。
 *
 * data/len  : SSE chunk 原始字节（不需要 NUL 结尾）
 * out       : 输出（调用方持有；C 只在本调用内写它）
 * sbuf/scap : 解码输出缓冲（content / reasoning_content / finish_reason）
 * sused     : 出参，缓冲区实际用量
 *
 * 返回 HA_CHUNK_OK / HA_CHUNK_FALLBACK / HA_CHUNK_TYPE_FAIL。
 *
 * ★ 调用方**必须**检查 choices_count：Go 侧是 `[]struct`，Unmarshal 会解析
 *   **全部**元素，而本层只取 [0]（协议约定）。若元素 >1，本层无法保证
 *   其余元素也能被 Go 解析（它们可能有类型错误）⇒ 必须回退。
 *   本函数在 choices_count>1 时**直接返回 FALLBACK**，不给调用方犯错的机会。
 */
int ha_sse_chunk_locate(const char *data, size_t len, ha_chunk_out *out,
                        char *sbuf, size_t scap, size_t *sused);

#ifdef __cplusplus
}
#endif

#endif /* HA_SSE_H */
