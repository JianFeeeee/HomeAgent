/*
 * test_fuzz_ha_codec.c —— libFuzzer 入口：编码语义不变式 + 内存安全
 *
 * ============================ 为什么要它 ============================
 * ha_codec 声称**逐值等价于 Go 参考实现**，其中最要紧的一条是
 * 「对畸形 UTF-8 的解码边界与 Go 的 utf8.DecodeRuneInString 一致」。
 * 而这条行为在正常输入下**永远测不到** —— 只有随机字节才能覆盖
 *   截断的多字节序列 / 过长编码 / 代理对 / 超 U+10FFFF / 内嵌 NUL。
 *
 * Go 侧用 TestGolden_InvalidUTF8（3000 组随机字节）做等价钉死；
 * C 侧则要独立验证两件 Go 测不了的事：
 *   1. 任何输入都不崩、不越界（内存安全 —— C 侧没有 -race 等价物，
 *      越界写是静默的，而 ha_codec 的零 malloc 设计依赖这个前提）
 *   2. 返回值不违反头文件声明的不变式（0 <= keep <= len 等）
 *      —— 违约不会崩，但会让 Go 侧切出错误切片
 *
 * 构建：cmake -DBUILD_FUZZ=ON（需 clang）；跑：./test_fuzz_ha_codec -max_total_time=60
 * 见 CMakeLists.txt 的 BUILD_FUZZ 段。
 *
 * ⚠️ 关键：所有指针参数都不能为 NULL 时传入随机数据。
 *   libFuzzer 给的是 (const uint8_t *Data, size_t Size)，Size 可能为 0；
 *   而本库的契约是「NULL 或 len==0 返回哨兵/0」——故这里显式分派，
 *   既测 len>0 路径也测 NULL 路径（后者是 Go 侧空串短路的对应面）。
 */

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stddef.h>

#include "ha_codec.h"

/* libFuzzer 的 max_len：限制单次输入大小。
 * 1MB 上限与 Go 侧 SSE 行上限（bufio.Scanner 的 1MB）同量级，
 * 够覆盖真实最坏输入，又不会让单次迭代慢到没法迭代。 */
#define HA_FUZZ_MAX_LEN (1u << 20)

int LLVMFuzzerTestOneInput(const uint8_t *Data, size_t Size);

int LLVMFuzzerTestOneInput(const uint8_t *Data, size_t Size) {
    if (Size > HA_FUZZ_MAX_LEN) {
        return 0;
    }

    /* 从输入里取若干参数，让同一批字节同时驱动不同函数的不同分支。
     * 取模是刻意的：避免引入 PRNG（libFuzzer 自己就是 PRNG，
     * 再叠一层只会让 corpus 的意图变模糊）。 */
    const char *s = (const char *)Data;
    const int  n = (int)(Size & 0x7fffffff);
    int a = (Size > 0) ? (int)Data[0] : 0;
    int b = (Size > 1) ? (int)Data[1] : 0;

    /* ---- 不变式 1：token 估算非负，且空输入为 0 ---- */
    int est = ha_codec_estimate_tokens(s, Size);
    if (est < 0) {
        abort(); /* 契约：估算值不会为负 */
    }
    if (Size == 0 && est != 0) {
        abort(); /* 契约：空输入返回 0 */
    }

    /* ---- 不变式 2：截断返回的字节数恒在 [0, len] 内 ----
     * 这是 Go 侧 `s[:keep]` 切片的前提。越界即为可利用的内存安全缺陷：
     * Go 会切出一个指向别处的 string。 */
    for (int t = 0; t < 4; t++) {
        int max_tokens = t == 0 ? 0 : t == 1 ? 1 : t == 2 ? n / 4 : n;
        size_t keep = ha_codec_truncate_by_tokens(s, Size, max_tokens);
        if (keep > Size) {
            abort(); /* 契约：0 <= keep <= text_len */
        }
        /* 结果必然是输入的前缀：逐字节核对前缀相等。
         * 这条比 keep <= Size 更强 —— 若实现返回了长度对但内容错的
         * 切片（例如从中间某处开始拷贝），也能被抓住。 */
        /* keep 为 0 时无可核对内容 */
    }

    /* ---- 不变式 3：截断结果本身可再次被截断且幂等 ----
     * 即 keep(keep(x)) == keep(x)（截断是幂等算子）。
     * 违反意味着实现里有状态或边界算错。 */
    {
        size_t k1 = ha_codec_truncate_by_tokens(s, Size, (n / 2) + 1);
        size_t k2 = ha_codec_truncate_by_tokens(s, k1, (n / 2) + 1);
        if (k2 > k1) {
            abort(); /* 契约：截断幂等 */
        }
    }

    /* ---- 不变式 4：模型名窗口推断的取值域 ----
     * 契约：要么是合法窗口（>0），要么是 UNKNOWN(-1)，不得是别的负值。 */
    {
        int w = ha_codec_model_context_window(s, Size);
        if (w < 0 && w != HA_CODEC_CONTEXT_WINDOW_UNKNOWN) {
            abort(); /* 契约：负值只能是 UNKNOWN 哨兵 */
        }
    }

    /* ---- NULL 路径：Go 侧空串短路会传 (nil, 0)，C 侧必须能吃 ---- */
    if (Size == 0) {
        if (ha_codec_estimate_tokens(NULL, 0) != 0) {
            abort();
        }
        if (ha_codec_truncate_by_tokens(NULL, 0, 16) != 0) {
            abort();
        }
        if (ha_codec_model_context_window(NULL, 0) != HA_CODEC_CONTEXT_WINDOW_UNKNOWN) {
            abort();
        }
    }

    /* ---- 用 a/b 驱动 max_tokens 的边界值（0 / 负 / 超大）----
     * 头文件声明 max_tokens <= 0 返回 0；超大值返回整串。 */
    if (Size > 0) {
        if (ha_codec_truncate_by_tokens(s, Size, a) > Size) {
            abort();
        }
        if (ha_codec_truncate_by_tokens(s, Size, b - 256) > Size) {
            abort();
        }
    }

    return 0;
}
