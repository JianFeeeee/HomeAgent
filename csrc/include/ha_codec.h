#ifndef HA_CODEC_H
#define HA_CODEC_H

/*
 * ha_codec — HomeAgent 内核编解码层（C 实现）
 *
 * ============================ 接口冻结声明 ============================
 * 本头文件是对外契约。函数签名、语义、返回值一经发布即为冻结接口，
 * 修改必须走大版本流程（与 third_party/homeagent-sdk 同一冻结标准）。
 *
 * 设计约束（见 docs/zh/c-core/llm-orchestration-c.md §四）：
 *   1. 只吃 const char* + 长度，出数值/JSON 串
 *   2. 不回调 Go、不传 Go 指针
 *   3. 不长期持有 malloc 内存；需要出参的用调用方缓冲区
 *   4. 无状态、纯函数、线程安全（不写全局可变状态）
 *
 * 当前覆盖：L1 协议编解码层中的纯计算部分（第一个最小切片）。
 */

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ==================== 模型上下文窗口推断 ==================== */

/* 无法从模型名推断时的哨兵值（与 Go 侧一致）。
 *
 * 为什么返回哨兵而不是直接给兜底值：调用方需要区分「真推断出了」与
 * 「推断不出、只能兜底」——后者要打一行日志（窗口被低估必须可见），
 * 并提示部署方用 per-source context_window 显式声明。
 * 若 C 侧直接返回兜底值，调用方就永远分不清这两种情况。 */
#define HA_CODEC_CONTEXT_WINDOW_UNKNOWN (-1)

/* 由模型名推断最大上下文窗口（token 数）；推断不出返回
 * HA_CODEC_CONTEXT_WINDOW_UNKNOWN。model 为 NULL 时同样返回 UNKNOWN。
 *
 * model 为 UTF-8 字符串，匹配大小写不敏感。
 * 语义必须与 Go 侧 modelContextWindowPure 逐值一致（黄金对照测试钉死）。 */
int ha_codec_model_context_window(const char *model);

/* ==================== token 估算与截断 ==================== */

/* 粗略估算 token 数。
 *
 * 规则（与 Go 侧 EstimateTokens 一致）：中文 ~1.5 token/字、英文 ~0.3 token/字符，
 * 保守取 max(1, runeCount * 2)。text 为 NULL 或空串返回 0。
 *
 * 注意：按 UTF-8 **字符数**（rune）计，不是字节数。 */
int ha_codec_estimate_tokens(const char *text);

/* 截断字符串至不超过 maxTokens 估计值，返回写入 out 的字节数（不含结尾 NUL）。
 *
 * 语义与 Go 侧 TruncateByTokens 一致：从开头保留 maxTokens/2 个字符。
 * maxTokens <= 0 或 text 为空时写入空串。
 *
 * out 由调用方提供，容量须为 outCap（含结尾 NUL）；函数保证 NUL 结尾、
 * 不越界写。返回值是实际写入的字节数（可能因 outCap 不足而短于完整截断结果）。 */
size_t ha_codec_truncate_by_tokens(const char *text, int max_tokens,
                                   char *out, size_t out_cap);

#ifdef __cplusplus
}
#endif

#endif /* HA_CODEC_H */
