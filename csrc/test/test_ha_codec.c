/*
 * test_ha_codec.c — ha_codec C 侧契约测试
 *
 * 编译运行（无 cmake 亦可）：
 *   gcc -std=c99 -I../include ../src/ha_codec.c test_ha_codec.c -o test_ha_codec && ./test_ha_codec
 *
 * 这一层钉死 C 实现的语义；与 Go 的逐值一致由黄金对照测试负责（双保险）。
 */

#include "ha_codec.h"

#include <stdio.h>
#include <string.h>

static int g_fail = 0;
static int g_pass = 0;

static void check_int(const char *what, int got, int want) {
    if (got != want) {
        printf("  [FAIL] %s: got %d, want %d\n", what, got, want);
        g_fail++;
    } else {
        g_pass++;
    }
}

static void check_str(const char *what, const char *got, const char *want) {
    if (strcmp(got, want) != 0) {
        printf("  [FAIL] %s: got \"%s\", want \"%s\"\n", what, got, want);
        g_fail++;
    } else {
        g_pass++;
    }
}

static void check_size(const char *what, size_t got, size_t want) {
    if (got != want) {
        printf("  [FAIL] %s: got %zu, want %zu\n", what, got, want);
        g_fail++;
    } else {
        g_pass++;
    }
}

static void test_context_window(void) {
    printf("model_context_window:\n");
    check_int("deepseek-v4.1-flash",
              ha_codec_model_context_window("deepseek/deepseek-v4.1-flash"), 1048576);
    check_int("deepseek-v4-flash",
              ha_codec_model_context_window("deepseek-v4-flash"), 1048576);
    check_int("deepseek-chat",
              ha_codec_model_context_window("deepseek-chat"), 65536);
    check_int("claude-opus-5",
              ha_codec_model_context_window("claude-opus-5"), 100000);
    check_int("gpt-4-turbo",
              ha_codec_model_context_window("gpt-4-turbo"), 128000);
    check_int("llama-3-70b",
              ha_codec_model_context_window("llama-3-70b"), 8192);
    check_int("AUTO (unknown)",
              ha_codec_model_context_window("AUTO"), HA_CODEC_CONTEXT_WINDOW_UNKNOWN);
    check_int("NULL (unknown)",
              ha_codec_model_context_window(NULL), HA_CODEC_CONTEXT_WINDOW_UNKNOWN);
    check_int("case-insensitive",
              ha_codec_model_context_window("QWEN-MAX"), 131072);
    check_int("moonshot",
              ha_codec_model_context_window("moonshot-v1-128k"), 131072);
    /* 分支顺序：gpt-4-turbo 必须先于裸 gpt-4 命中 */
    check_int("gpt-4-mini (branch order)",
              ha_codec_model_context_window("gpt-4-mini"), 128000);
    check_int("gpt-4 (bare)",
              ha_codec_model_context_window("gpt-4"), 8192);
    /* claude-3 必须先于裸 claude */
    check_int("claude-3-opus (branch order)",
              ha_codec_model_context_window("claude-3-opus"), 200000);
}

static void test_estimate_tokens(void) {
    printf("estimate_tokens:\n");
    check_int("empty", ha_codec_estimate_tokens(""), 0);
    check_int("NULL", ha_codec_estimate_tokens(NULL), 0);
    /* "abc" = 3 rune * 2 = 6 */
    check_int("ascii abc", ha_codec_estimate_tokens("abc"), 6);
    /* "你好" = 2 rune * 2 = 4（注意：不是字节数 6） */
    check_int("chinese 2 chars", ha_codec_estimate_tokens("你好"), 4);
    /* 混合 "a你" = 2 rune * 2 = 4 */
    check_int("mixed", ha_codec_estimate_tokens("a你"), 4);
    /* 4 字节 emoji：1 rune * 2 = 2 */
    check_int("emoji", ha_codec_estimate_tokens("\xF0\x9F\x98\x80"), 2);
}

static void test_truncate(void) {
    printf("truncate_by_tokens:\n");
    char buf[64];

    /* max_tokens<=0 → 空 */
    ha_codec_truncate_by_tokens("hello", 0, buf, sizeof(buf));
    check_str("max_tokens=0", buf, "");

    /* 未超限 → 原样返回 */
    size_t n = ha_codec_truncate_by_tokens("abc", 100, buf, sizeof(buf));
    check_str("no truncation", buf, "abc");
    check_size("no truncation len", n, 3);

    /* "abcdefghij" = 10 rune → 20 tokens；max=8 → keep=4 → "abcd" */
    n = ha_codec_truncate_by_tokens("abcdefghij", 8, buf, sizeof(buf));
    check_str("keep 4", buf, "abcd");
    check_size("keep 4 len", n, 4);

    /* 中文按 rune 截断，不切碎 UTF-8："你好世界" 4 rune，max=4 → keep=2 → "你好" */
    n = ha_codec_truncate_by_tokens("你好世界", 4, buf, sizeof(buf));
    check_str("chinese keep 2", buf, "你好");
    check_size("chinese keep 2 len (bytes)", n, 6);

    /* 缓冲区不足：必须 NUL 结尾且不越界 */
    char tiny[4];
    n = ha_codec_truncate_by_tokens("abcdefghij", 100, tiny, sizeof(tiny));
    check_size("tiny buf len", n, 3);
    check_str("tiny buf NUL-terminated", tiny, "abc");

    /* out_cap=0 不写 */
    check_size("zero cap", ha_codec_truncate_by_tokens("abc", 100, tiny, 0), 0);
}

int main(void) {
    printf("=== ha_codec 契约测试 ===\n\n");
    test_context_window();
    test_estimate_tokens();
    test_truncate();
    printf("\n=== 结果: %d passed, %d failed ===\n", g_pass, g_fail);
    return g_fail == 0 ? 0 : 1;
}
