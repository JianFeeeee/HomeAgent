/*
 * test_ha_codec.c — ha_codec C 侧契约测试
 *
 * 编译运行（无 cmake 亦可）：
 *   gcc -std=c99 -I../include ../src/ha_codec.c test_ha_codec.c -o test_ha_codec && ./test_ha_codec
 *
 * 这一层钉死 C 实现的语义；与 Go 的逐值一致由黄金对照测试负责（双保险）。
 *
 * ★ 注意签名已改为「指针 + 长度」（见 ha_codec.h）：不再依赖 NUL 结尾，
 *   截断返回字节数而非字符串。测试相应用 LIT()/LEN 辅助宏。
 */

#include "ha_codec.h"

#include <stdio.h>
#include <string.h>

static int g_fail = 0;
static int g_pass = 0;

/* 字面量 → (指针, 长度)：避免每处手写 sizeof-1。 */
#define LIT(s) (s), (sizeof(s) - 1)

static void check_int(const char *what, int got, int want) {
    if (got != want) {
        printf("  [FAIL] %s: got %d, want %d\n", what, got, want);
        g_fail++;
    } else {
        g_pass++;
    }
}

/* 断言「截断得到的字节数」确实是原文前缀，且正好是期望的字节长度。 */
static void check_trunc_prefix(const char *what, const char *text, size_t len,
                               int max_tokens, size_t want_bytes) {
    size_t got = ha_codec_truncate_by_tokens(text, len, max_tokens);
    if (got != want_bytes) {
        printf("  [FAIL] %s: got %zu bytes, want %zu\n", what, got, want_bytes);
        g_fail++;
        return;
    }
    if (got > len) {
        printf("  [FAIL] %s: 返回值 %zu 超出输入长度 %zu\n", what, got, len);
        g_fail++;
        return;
    }
    g_pass++;
}

/* 字节级断言：截断结果的字节内容必须与期望字符串逐字节相等。 */
static void check_trunc_bytes(const char *what, const char *text, size_t len,
                              int max_tokens, const char *want) {
    size_t got = ha_codec_truncate_by_tokens(text, len, max_tokens);
    size_t want_len = strlen(want);
    if (got != want_len) {
        printf("  [FAIL] %s: got %zu bytes, want %zu\n", what, got, want_len);
        g_fail++;
        return;
    }
    if (got > 0 && memcmp(text, want, got) != 0) {
        printf("  [FAIL] %s: 字节内容不匹配\n", what);
        g_fail++;
        return;
    }
    g_pass++;
}

static void test_context_window(void) {
    printf("model_context_window:\n");
    check_int("deepseek-v4.1-flash",
              ha_codec_model_context_window(LIT("deepseek/deepseek-v4.1-flash")), 1048576);
    check_int("deepseek-v4-flash",
              ha_codec_model_context_window(LIT("deepseek-v4-flash")), 1048576);
    check_int("deepseek-chat",
              ha_codec_model_context_window(LIT("deepseek-chat")), 65536);
    check_int("claude-opus-5",
              ha_codec_model_context_window(LIT("claude-opus-5")), 100000);
    check_int("gpt-4-turbo",
              ha_codec_model_context_window(LIT("gpt-4-turbo")), 128000);
    check_int("llama-3-70b",
              ha_codec_model_context_window(LIT("llama-3-70b")), 8192);
    check_int("AUTO (unknown)",
              ha_codec_model_context_window(LIT("AUTO")), HA_CODEC_CONTEXT_WINDOW_UNKNOWN);
    check_int("NULL (unknown)",
              ha_codec_model_context_window(NULL, 0), HA_CODEC_CONTEXT_WINDOW_UNKNOWN);
    check_int("zero len (unknown)",
              ha_codec_model_context_window("abc", 0), HA_CODEC_CONTEXT_WINDOW_UNKNOWN);
    check_int("case-insensitive",
              ha_codec_model_context_window(LIT("QWEN-MAX")), 131072);
    check_int("moonshot",
              ha_codec_model_context_window(LIT("moonshot-v1-128k")), 131072);
    /* 分支顺序：gpt-4-turbo 必须先于裸 gpt-4 命中 */
    check_int("gpt-4-mini (branch order)",
              ha_codec_model_context_window(LIT("gpt-4-mini")), 128000);
    check_int("gpt-4 (bare)",
              ha_codec_model_context_window(LIT("gpt-4")), 8192);
    /* claude-3 必须先于裸 claude */
    check_int("claude-3-opus (branch order)",
              ha_codec_model_context_window(LIT("claude-3-opus")), 200000);
    /* 中文子串：非 ASCII 字节不受折叠影响 */
    check_int("零一万物",
              ha_codec_model_context_window(LIT("\xe9\x9b\xb6\xe4\xb8\x80\xe4\xb8\x87\xe7\x89\xa9")), 200000);

    /* 非 NUL 结尾：把模型名放在大缓冲中间，只传前 N 字节。
     * 这是新签名的关键能力（旧签名会读到后续垃圾）。 */
    {
        char buf[64];
        memset(buf, 'Z', sizeof(buf));
        memcpy(buf, "qwen-max", 8);
        check_int("no NUL terminator (prefix only)",
                  ha_codec_model_context_window(buf, 8), 131072);
    }

    /* 超长模型名（超过栈缓冲）必须仍零分配地正确匹配。 */
    {
        static char big[512];
        memset(big, 'a', sizeof(big));
        memcpy(big + 400, "gpt-4-turbo", 11);
        check_int("oversize model name (heap-free fallback)",
                  ha_codec_model_context_window(big, sizeof(big)), 128000);
    }
}

static void test_estimate_tokens(void) {
    printf("estimate_tokens:\n");
    check_int("empty", ha_codec_estimate_tokens(LIT("")), 0);
    check_int("NULL", ha_codec_estimate_tokens(NULL, 0), 0);
    check_int("zero len", ha_codec_estimate_tokens("abc", 0), 0);
    /* "abc" = 3 rune * 2 = 6 */
    check_int("ascii abc", ha_codec_estimate_tokens(LIT("abc")), 6);
    /* "你好" = 2 rune * 2 = 4（不是字节数 6） */
    check_int("chinese 2 chars", ha_codec_estimate_tokens(LIT("你好")), 4);
    /* 混合 "a你" = 2 rune * 2 = 4 */
    check_int("mixed", ha_codec_estimate_tokens(LIT("a你")), 4);
    /* 4 字节 emoji：1 rune * 2 = 2 */
    check_int("emoji", ha_codec_estimate_tokens(LIT("\xF0\x9F\x98\x80")), 2);

    /* ASCII 快路径跨界：长度正好落在批量块边界附近，计数必须精确。 */
    {
        static char buf[300];
        memset(buf, 'x', sizeof(buf));
        check_int("ascii 300 bytes (chunk boundaries)",
                  ha_codec_estimate_tokens(buf, sizeof(buf)), 600);
    }
    /* 非 NUL 结尾：只计前 N 字节（后面是垃圾）。 */
    {
        char buf[16];
        memcpy(buf, "abc", 3);
        memset(buf + 3, 'x', sizeof(buf) - 3);
        check_int("no NUL terminator (prefix only)",
                  ha_codec_estimate_tokens(buf, 3), 6);
    }
    /* 截断的多字节序列：Go 对无效序列按每字节 1 rune 计，C 必须一致。 */
    check_int("truncated 3-byte seq (invalid)",
              ha_codec_estimate_tokens("\xE4\xBD", 2), 4); /* 2 rune → 4 */
}

static void test_truncate(void) {
    printf("truncate_by_tokens:\n");

    /* max_tokens<=0 → 0 字节 */
    check_trunc_prefix("max_tokens=0", LIT("hello"), 0, 0);

    /* 未超限 → 全长 */
    check_trunc_prefix("no truncation", LIT("abc"), 100, 3);

    /* "abcdefghij" = 10 rune → 20 tokens；max=8 → keep=4 → "abcd" */
    check_trunc_prefix("keep 4", LIT("abcdefghij"), 8, 4);

    /* 中文按 rune 截断，不切碎 UTF-8："你好世界" 4 rune，max=4 → keep=2 → "你好"(6B) */
    check_trunc_prefix("chinese keep 2", LIT("你好世界"), 4, 6);

    /* 恰好等于预算：不截断 */
    check_trunc_prefix("exact budget", LIT("abc"), 6, 3);
    /* 差一：截断。3 rune=6 tokens，max=5 → keep=2 → "ab" */
    check_trunc_prefix("just under budget", LIT("abc"), 5, 2);

    /* 长 ASCII 跨批量块边界，keep 落在块内（提前短路路径）。 */
    {
        static char buf[200];
        memset(buf, 'k', sizeof(buf));
        check_trunc_prefix("long ascii, keep inside chunk", buf, sizeof(buf), 128, 64);
    }

    /* 非 NUL 结尾：max 足够大 → 返回传入长度（而非 strlen 结果）。 */
    {
        char buf[16];
        memcpy(buf, "abcd", 4);
        memset(buf + 4, 'x', sizeof(buf) - 4);
        check_trunc_prefix("no NUL terminator, full length", buf, 4, 100, 4);
    }

    /* 单字节 rune 边界：ASCII 与多字节混合，确保不切在字符中间。
     * "a你b好c" = 5 rune = 10 tokens；max=6 → keep=3 → "a你b" = 1+3+1 = 5 字节 */
    check_trunc_prefix("mixed keep 3", LIT("a你b好c"), 6, 5);
    /* max=4 → keep=2 → "a你" = 1+3 = 4 字节（正好切在字符边界上）*/
    check_trunc_prefix("mixed keep 2 (byte boundary)", LIT("a你b好c"), 4, 4);
    /* 字节内容级校验：结果必须是原串的**逐字节前缀**，不能切碎 UTF-8。 */
    check_trunc_bytes("content zh keep 2", "你好世界", sizeof("你好世界") - 1, 4, "你好");
    check_trunc_bytes("content ascii keep 4", "abcdefghij", 10, 8, "abcd");
    check_trunc_bytes("content no truncation", "abc", 3, 100, "abc");
}

int main(void) {
    printf("=== ha_codec 契约测试 ===\n\n");
    test_context_window();
    test_estimate_tokens();
    test_truncate();
    printf("\n=== 结果: %d passed, %d failed ===\n", g_pass, g_fail);
    return g_fail == 0 ? 0 : 1;
}
