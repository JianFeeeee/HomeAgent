/*
 * ha_codec.c — HomeAgent 内核编解码层（C 实现）
 *
 * 第一个最小切片：模型窗口推断 + token 估算/截断。
 * 语义必须与 Go 侧实现逐值一致，由黄金对照测试钉死。
 */

#include "ha_codec.h"

#include <ctype.h>
#include <stdlib.h>
#include <string.h>

/* ---------------------------------------------------------------- */
/* 小工具                                                            */
/* ---------------------------------------------------------------- */

/* 在 s 中查找子串 sub（子串已小写）。s 需已是小写。找不到返回 NULL。 */
static const char *find_sub(const char *s, const char *sub) {
    return strstr(s, sub);
}

/* 分配一份小写副本。调用方负责 free。失败返回 NULL。 */
static char *lower_dup(const char *s) {
    if (s == NULL) {
        return NULL;
    }
    size_t n = strlen(s);
    char *p = (char *)malloc(n + 1);
    if (p == NULL) {
        return NULL;
    }
    for (size_t i = 0; i < n; i++) {
        /* 只对 ASCII 做小写；UTF-8 多字节原样保留（与 Go strings.ToLower 对
         * 中文不改变结果一致——Go 会把非 ASCII 也处理，但模型名都是 ASCII）。 */
        unsigned char c = (unsigned char)s[i];
        p[i] = (char)((c < 0x80) ? tolower(c) : c);
    }
    p[n] = '\0';
    return p;
}

/* ---------------------------------------------------------------- */
/* 模型上下文窗口推断                                                */
/* ---------------------------------------------------------------- */

int ha_codec_model_context_window(const char *model) {
    if (model == NULL) {
        return HA_CODEC_CONTEXT_WINDOW_UNKNOWN;
    }

    char *m = lower_dup(model);
    if (m == NULL) {
        return HA_CODEC_CONTEXT_WINDOW_UNKNOWN;
    }

    int result = HA_CODEC_CONTEXT_WINDOW_UNKNOWN;

    /* 顺序与 Go 侧 switch 分支**严格一致**：先匹配到的分支胜出。
     * 这不是「随便一组 if」，顺序错了就会给出不同窗口。 */
    if (find_sub(m, "deepseek-v4") || find_sub(m, "deepseek-v3")) {
        result = 1048576;
    } else if (find_sub(m, "deepseek-r1") || find_sub(m, "deepseek-chat")) {
        result = 65536;
    } else if (find_sub(m, "gpt-4") &&
               (find_sub(m, "turbo") || find_sub(m, "mini") || find_sub(m, "omni"))) {
        result = 128000;
    } else if (find_sub(m, "gpt-4")) {
        result = 8192;
    } else if (find_sub(m, "gpt-3.5")) {
        result = 16384;
    } else if (find_sub(m, "claude-3.5") || find_sub(m, "claude-3")) {
        result = 200000;
    } else if (find_sub(m, "claude")) {
        result = 100000;
    } else if (find_sub(m, "gemini-1.5") || find_sub(m, "gemini-2")) {
        result = 1048576;
    } else if (find_sub(m, "gemini")) {
        result = 32768;
    } else if (find_sub(m, "qwen")) {
        result = 131072;
    } else if (find_sub(m, "glm") || find_sub(m, "chatglm")) {
        result = 131072;
    } else if (find_sub(m, "llama-3")) {
        result = 8192;
    } else if (find_sub(m, "llama-2")) {
        result = 4096;
    } else if (find_sub(m, "mistral") || find_sub(m, "mixtral")) {
        result = 32768;
    } else if (find_sub(m, "yi-") || find_sub(m, "零一")) {
        result = 200000;
    } else if (find_sub(m, "moonshot") || find_sub(m, "kimi")) {
        result = 131072;
    }

    free(m);
    return result;
}

/* ---------------------------------------------------------------- */
/* token 估算                                                        */
/* ---------------------------------------------------------------- */

/* 计 UTF-8 字符数（rune 数）并返回下一字符起点。
 * 非法字节按 1 字符前进（不吞字节），保证不会死循环。 */
static size_t utf8_next(const char *s, size_t remaining) {
    unsigned char c = (unsigned char)s[0];
    size_t len = 1;
    if (c >= 0xF0 && remaining >= 4) {
        len = 4;
    } else if (c >= 0xE0 && remaining >= 3) {
        len = 3;
    } else if (c >= 0xC0 && remaining >= 2) {
        len = 2;
    }
    return len;
}

int ha_codec_estimate_tokens(const char *text) {
    if (text == NULL || text[0] == '\0') {
        return 0;
    }

    size_t n = strlen(text);
    size_t runes = 0;
    size_t i = 0;
    while (i < n) {
        i += utf8_next(text + i, n - i);
        runes++;
    }

    /* 与 Go 侧一致：t = runeCount * 2；t < 1 时取 1。
     * runes > 0 时 t >= 2，故只需处理溢出与下限。 */
    if (runes > (size_t)0x3FFFFFFF) { /* 防 int 溢出 */
        return 0x7FFFFFFF;
    }
    int t = (int)(runes * 2);
    if (t < 1) {
        return 1;
    }
    return t;
}

/* ---------------------------------------------------------------- */
/* 按 token 截断                                                     */
/* ---------------------------------------------------------------- */

size_t ha_codec_truncate_by_tokens(const char *text, int max_tokens,
                                   char *out, size_t out_cap) {
    if (out == NULL || out_cap == 0) {
        return 0;
    }
    out[0] = '\0';

    if (max_tokens <= 0 || text == NULL || text[0] == '\0') {
        return 0;
    }

    size_t n = strlen(text);

    /* 先算 rune 数：与 Go 侧 len([]rune(s))*2 <= maxTokens 的短路一致 */
    size_t runes = 0;
    size_t i = 0;
    while (i < n) {
        i += utf8_next(text + i, n - i);
        runes++;
    }

    /* 未超限：整体返回 */
    if (runes <= (size_t)0x3FFFFFFF && (int)(runes * 2) <= max_tokens) {
        size_t copy = (n < out_cap - 1) ? n : (out_cap - 1);
        memcpy(out, text, copy);
        out[copy] = '\0';
        return copy;
    }

    /* 保留 maxTokens/2 个字符（与 Go 一致：keep := maxTokens / 2，整数除法） */
    size_t keep = (size_t)(max_tokens / 2);

    size_t byte_end = 0;
    size_t kept = 0;
    while (kept < keep && byte_end < n) {
        byte_end += utf8_next(text + byte_end, n - byte_end);
        kept++;
    }

    size_t copy = (byte_end < out_cap - 1) ? byte_end : (out_cap - 1);
    memcpy(out, text, copy);
    out[copy] = '\0';
    return copy;
}
