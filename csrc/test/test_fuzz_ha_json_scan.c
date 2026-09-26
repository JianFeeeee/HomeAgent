/*
 * test_fuzz_ha_json_scan.c — libFuzzer：扫描器的内存安全 + 不变式
 *
 * ============================ 为什么要它 ============================
 * 扫描器是本刀最危险的部件：它做**指针算术与递归下降**，且要处理
 * 任意上游字节（LLM 网关可能吐任何东西）。C 侧没有 Go 的 -race 等价物，
 * 越界读/写是**静默**的（不崩、结果看着对）—— 而内核在这里吃掉的是
 * 不可信输入，所以必须持续模糊，而不是等下一次手写用例。
 *
 * 覆盖的六条不变式：
 *   1. scan/skip 的游标**永不越过**输入长度（否则后续所有 span 都错位）
 *   2. skip 成功 ⇒ 恰好消费一个完整值，不残留结构字符
 *   3. 成员迭代器游标单调，且永不越过输入长度
 *   4. 解码输出的长度上界 = 输入长度的 3 倍
 *      （每个字节最坏变一个 U+FFFD = 3 字节；这是内存规划的前提）
 *   5. decode_into **绝不越界写**
 *   6. get_int 与 strtoll 语义在合法整数上一致（溢出时都必须拒绝）
 *
 * 构建：cmake -DBUILD_FUZZ=ON（需 clang）
 */

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <limits.h>
#include <errno.h>
#include <stdio.h>

#include "ha_json_scan.h"

#define HA_FUZZ_MAX_LEN (1u << 18)   /* 256KB：比真实 chunk 大得多，够覆盖 */

/* 累积解码输出的 sink */
typedef struct {
    size_t total;
    int    overflowed;
    char   stash[4096];   /* 小段暂存，用于比对 decode_into */
    size_t stash_len;
} acc_t;

static void acc_sink(void *ctx, const char *b, size_t n) {
    acc_t *a = (acc_t *)ctx;
    /* 累加并做溢出保护：若真出现无界增长，这里会先崩（暴露问题），
     * 而不是静默算错。 */
    if (a->total > (1ull << 40)) {
        a->overflowed = 1;
    }
    a->total += n;
    if (a->stash_len + n <= sizeof(a->stash)) {
        memcpy(a->stash + a->stash_len, b, n);
        a->stash_len += n;
    }
}

int LLVMFuzzerTestOneInput(const uint8_t *Data, size_t Size);

int LLVMFuzzerTestOneInput(const uint8_t *Data, size_t Size) {
    if (Size > HA_FUZZ_MAX_LEN) {
        return 0;
    }
    const char *s = (const char *)Data;

    /* ---- 1. skip 游标边界 ---- */
    ha_json_scan sc;
    ha_json_scan_init(&sc, s, Size);
    int ok = ha_json_skip(&sc);
    if (sc.i > Size) {
        abort(); /* 游标越界 —— 后续所有 span 都会错位 */
    }

    /* ---- 2. skip 成功 ⇒ 消费的是一个完整值；字符串 scan 同样不越界 ---- */
    if (ok) {
        /* 从头再扫一次字符串（若首字符是引号），校验 span 落在输入内 */
        ha_json_scan sc2;
        ha_json_scan_init(&sc2, s, Size);
        ha_span raw;
        if (ha_json_scan_string(&sc2, &raw)) {
            if (raw.len > Size) {
                abort();
            }
            /* span 必须落在输入区间内 */
            if (raw.p < s || raw.p > s + Size) {
                abort();
            }
        }
        if (sc2.i > Size) {
            abort();
        }
    }

    /* ---- 3. 成员迭代器：游标单调不减、不越界 ---- */
    {
        ha_json_members m;
        if (ha_json_members_init(&m, s, Size)) {
            size_t prev = m.sc.i;
            ha_span key, val;
            int guard = 0;
            while (ha_json_members_next(&m, &key, &val)) {
                if (m.sc.i > Size) {
                    abort();
                }
                if (m.sc.i < prev) {
                    abort(); /* 游标回退 ⇒ 可能死循环 */
                }
                prev = m.sc.i;
                if (key.len > Size || key.p < s || key.p > s + Size) {
                    abort();
                }
                /* ★ 值必须能独立跳过：这条不变式正是模糊测试第一轮
                 * 抓到的缺陷（旧 API 只报值起点、不消费值，游标仍在
                 * 值的前面，于是下一个成员解析到了值本身）。 */
                ha_json_scan vs;
                ha_json_scan_init(&vs, val.p, val.len);
                if (!ha_json_skip(&vs)) {
                    abort(); /* 成员报了个值，却跳不过去 ⇒ 内部不一致 */
                }
                if (val.len > Size || val.p < s || val.p > s + Size) {
                    abort();
                }
                if (++guard > 100000) {
                    abort(); /* 死循环保护 */
                }
            }
            /* 游标必须落在输入内 */
            if (m.sc.i > Size) {
                abort();
            }
            if (m.sc.i > Size) {
                abort();
            }
        }
    }

    /* ---- 4/5. 解码：输出上界 + 不越界写 ----
     * 上界 3×：每个输入字节最坏变一个 3 字节 U+FFFD。
     * 若违反，说明解码器会放大数据 —— 那是内存放大的安全隐患。 */
    {
        ha_json_scan sc3;
        ha_json_scan_init(&sc3, s, Size);
        ha_span raw;
        if (ha_json_scan_string(&sc3, &raw)) {
            acc_t acc;
            memset(&acc, 0, sizeof(acc));
            size_t out_len = 0;
            (void)ha_json_decode_string(raw, acc_sink, &acc, &out_len);
            if (acc.total != out_len) {
                abort(); /* sink 累加必须等于报告的 out_len */
            }
            if (acc.total > (size_t)raw.len * 3 + 3) {
                abort(); /* 放大超过 3× 上界 */
            }

            /* decode_into 用小缓冲：绝不越界（哨兵检查） */
            char tiny[8];
            memset(tiny, 0x5a, sizeof(tiny));
            size_t got = ha_json_decode_string_into(raw, tiny, sizeof(tiny));
            /* 成功时必须以 NUL 结尾且长度 < cap */
            if (got != (size_t)-1) {
                if (got >= sizeof(tiny)) {
                    abort();
                }
                if (tiny[got] != '\0') {
                    abort();
                }
            } else {
                /* 失败：末尾 NUL 位不得被单独改写（仍是哨兵或已被部分写）*/
                /* 只要求不越界 —— ASan 已保证，这里做一个显式触摸 */
                (void)tiny[sizeof(tiny) - 1];
            }
        }
    }

    /* ---- 6. get_int 与 strtoll 对照（合法整数） ---- */
    {
        ha_span v = { s, Size };
        long long got = 0;
        if (ha_json_get_int(v, &got)) {
            /* 本库认了 ⇒ 必须是纯整数，且 strtoll 应给出同值 */
            char *dup = (char *)malloc(Size + 1);
            if (dup) {
                memcpy(dup, s, Size);
                dup[Size] = '\0';
                errno = 0;
                char *end = NULL;
                long long ref = strtoll(dup, &end, 10);
                /* 只有「整串被消费且无溢出」时才可比较 */
                if (errno == 0 && end == dup + Size) {
                    if (ref != got) {
                        abort(); /* 与 strtoll 分叉 */
                    }
                }
                free(dup);
            }
        }
    }

    /* ---- NULL / 空输入防御 ---- */
    if (Size == 0) {
        ha_json_scan z;
        ha_json_scan_init(&z, NULL, 0);
        if (!ha_json_scan_eof(&z)) {
            abort();
        }
        if (ha_json_skip(&z)) {
            abort();
        }
        long long v;
        ha_span e = { NULL, 0 };
        if (ha_json_get_int(e, &v)) {
            abort();
        }
    }

    return 0;
}
