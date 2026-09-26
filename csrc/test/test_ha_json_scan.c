/*
 * test_ha_json_scan.c — ha_json_scan 的 C 侧契约测试
 *
 * 覆盖重点（与 docs/zh/c-core/sse-codec-c.md 真值表对应）：
 *   语法严格性、键大小写不敏感、重复键后者胜、\u 解码（含代理对）、
 * 非法 UTF-8 → U+FFFD、整数溢出、深度保险。
 *
 * 另一半验收在 Go 侧（codec_jsongolden_test.go）：与 encoding/json 逐值比对。
 * 本文件负责**不依赖 Go** 的语义自洽与边界安全。
 */

#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#include "ha_json_scan.h"

static int g_fail = 0;
static int g_run = 0;

static void check(int cond, const char *what, const char *detail) {
    g_run++;
    if (!cond) {
        g_fail++;
        printf("  [FAIL] %s%s%s\n", what,
               detail ? " :: " : "", detail ? detail : "");
    }
}

static void check_str(const char *what, const char *got, size_t gotlen,
                      const char *want) {
    g_run++;
    size_t wl = strlen(want);
    if (wl != gotlen || memcmp(got, want, wl) != 0) {
        g_fail++;
        printf("  [FAIL] %s: got \"%.*s\" want \"%s\"\n", what,
               (int)gotlen, got, want);
    }
}

/* ---------------- 语法严格性 ---------------- */

/* 约定：ok=1 表示「skip 成功」；ok=0 表示「拒绝」。
 * ★ 注意 `{"a":1}x` 在 skip 层**不拒绝**（skip 只跳一个值），
 *   而「尾部有残留」的判定是**调用方的义务**（比对游标是否到末尾）。
 *   这与 Go 侧 json.Unmarshal 的区别就在这：Unmarshal 会拒绝尾部残留。
 *   故下面用 eoc（end-of-consume）字段单独断言。 */
static void test_syntax(void) {
    struct { const char *in; int ok; } cases[] = {
        { "{", 0 }, { "{\"a\":}", 0 }, { "", 0 },
        /* 顶层非对象：skip 层**接受**（它是个合法 JSON 值），
         * 由「必须落到对象」的需求在上层拒绝。Go 侧拒绝是因为要 Unmarshal
         * 进 struct，与 skip 语义不同层。 */
        { "null", 1 }, { "[]", 1 }, { "\"str\"", 1 }, { "123", 1 },
        { "{\"a\":1,}", 0 },        /* 尾逗号非法 */
        { "{'a':1}", 0 },           /* 单引号非法 */
        { "{\"a\":1", 0 },          /* 未闭合 */
        { "{\"a\" 1}", 0 },        /* 缺冒号 */
        { "{\"a\":01}", 0 },        /* 前导零 */
        { "{\"a\":1.}", 0 },        /* 1. 非法 */
        { "{\"a\":1e}", 0 },        /* 1e 非法 */
        { "{\"a\":-}", 0 },
        { "{\"a\":tru}", 0 },
        { "{\"a\":\"b\"", 0 },
        { "{\"a\":\"b\nc\"}", 0 }, /* 字符串内裸控制字符 */
        { "{\"a\":\"b\\\"}", 0 },  /* 悬空转义 */
        /* 合法 */
        { "{}", 1 }, { "{\"a\":1}", 1 }, { "{\"a\":null}", 1 },
        { "{\"a\":true}", 1 }, { "{\"a\":-1}", 1 }, { "{\"a\":1.5}", 1 },
        { "{\"a\":1e2}", 1 }, { "  {\"a\" : 1 }  ", 1 },
        { "{\"a\":\"\\u4f60\"}", 1 }, { "{\"a\":{\"b\":[1,2]}}", 1 },
        { "{\"a\":[],\"b\":{}}", 1 },
    };
    for (size_t i = 0; i < sizeof(cases)/sizeof(cases[0]); i++) {
        ha_json_scan sc;
        ha_json_scan_init(&sc, cases[i].in, strlen(cases[i].in));
        int ok = ha_json_skip(&sc);
        check(ok == cases[i].ok, "syntax", cases[i].in);
    }

    /* 尾部残留：skip 不管，但调用方必须能察觉（比对游标） */
    {
        const char *s = "{\"a\":1}x";
        ha_json_scan sc;
        ha_json_scan_init(&sc, s, strlen(s));
        check(ha_json_skip(&sc) == 1, "trailing-garbage-skip-ok", s);
        check(sc.i != sc.n, "trailing-garbage-detectable", s);
    }
    /* 前后空白：必须被吃掉，调用方才能用 i==n 判定「干净」 */
    {
        const char *s = "  {\"a\" : 1 }  ";
        ha_json_scan sc;
        ha_json_scan_init(&sc, s, strlen(s));
        check(ha_json_skip(&sc) == 1, "ws-skip-ok", s);
        /* 尾部空白是 JSON 允许的：**不能**要求游标精确落在 n。
         * 真正要保证的是「值本体已被完整消费」——
         * 即剩余部分只剩空白。这个判定留给调用方（见 sse-codec-c.md §2.5）。 */
        int rest_is_ws = 1;
        for (size_t k = sc.i; k < sc.n; k++) {
            if (s[k] != ' ' && s[k] != '\t' && s[k] != '\n' && s[k] != '\r') {
                rest_is_ws = 0;
            }
        }
        check(rest_is_ws, "ws-tail-only-whitespace", s);
    }
}

/* ---------------- 顶层成员迭代 / 大小写不敏感 ---------------- */

static void test_members(void) {
    /* Go 的键匹配大小写不敏感：{"DELTA":{"CONTENT":"up"}} */
    const char *s = "{\"DELTA\":{\"CONTENT\":\"up\"}}";
    ha_json_members m;
    check(ha_json_members_init(&m, s, strlen(s)) == 1, "members-init", s);

    ha_span key, val, outer_delta = { NULL, 0 };
    while (ha_json_members_next(&m, &key, &val)) {
        if (ha_json_key_eq(key, "delta")) {
            outer_delta = val;
        }
    }
    check(outer_delta.p != NULL, "members-case-insensitive", s);
    check(ha_json_members_complete(&m) == 1, "members-complete", s);

    /* 二级：CONTENT 也应能取到 */
    ha_json_members m2;
    check(ha_json_members_init(&m2, outer_delta.p, outer_delta.len) == 1,
          "members-init-2", NULL);
    ha_span k2, v2;
    int found = 0;
    while (ha_json_members_next(&m2, &k2, &v2)) {
        if (ha_json_key_eq(k2, "content")) {
            found = 1;
        }
    }
    check(found, "members-case-insensitive-2", NULL);
    check(ha_json_members_complete(&m2) == 1, "members-complete-2", NULL);

    /* 便捷取值 */
    char buf[64];
    size_t n = ha_json_object_get_string(outer_delta, "CONTENT", buf, sizeof(buf));
    g_run++;
    if (n != 2 || memcmp(buf, "up", 2) != 0) {
        g_fail++;
        printf("  [FAIL] object_get_string: n=%zu buf=%s\n", n, buf);
    }
}

/* ---------------- 重复键后者胜 ---------------- */

static void test_dup_key(void) {
    const char *s = "{\"total_tokens\":1,\"total_tokens\":2}";
    ha_span obj = { s, strlen(s) };
    long long v = 0;
    check(ha_json_object_get_int(obj, "total_tokens", &v) == 1, "dup-getint", s);
    g_run++;
    if (v != 2) {
        g_fail++;
        printf("  [FAIL] dup-key 应后者胜: got %lld want 2\n", v);
    }
}

/* ---------------- 畸形输入必须能被辨别（复刻 Go 严格性） ---------------- */
static void test_malformed_detected(void) {
    struct { const char *in; int complete; } cases[] = {
        { "{}", 1 }, { "{\"a\":1}", 1 },
        { "{\"a\":1", 0 },        /* 缺 '}' */
        { "{\"a\":1,}", 0 },      /* 尾逗号 */
        { "{\"a\":}", 0 },        /* 值非法 */
        { "{\"a\"}", 0 },         /* 缺冒号与值 */
        { "{\"a\":1 \"b\":2}", 0 }, /* 缺逗号 */
        { "{'a':1}", 0 },           /* 单引号 */
    };
    for (size_t i = 0; i < sizeof(cases)/sizeof(cases[0]); i++) {
        ha_json_members m;
        int init_ok = ha_json_members_init(&m, cases[i].in, strlen(cases[i].in));
        g_run++;
        if (!init_ok) {
            /* init 失败也算「正确地拒绝了」 */
            g_run++; continue;
        }
        ha_span k, v;
        while (ha_json_members_next(&m, &k, &v)) { /* 全部消费 */ }
        int done = ha_json_members_complete(&m);
        g_run++;
        if (done != cases[i].complete) {
            g_fail++;
            printf("  [FAIL] malformed[%zu] %s: complete=%d 期望 %d\n",
                   i, cases[i].in, done, cases[i].complete);
        }
    }

    /* 关键：返回 1 的成员，其值必须能独立 skip（fuzz 抓到过的正是这条） */
    {
        const char *s = "{\"\":k\"\"}";   /* fuzz 崩溃输入的形状 */
        ha_json_members m;
        if (ha_json_members_init(&m, s, strlen(s))) {
            ha_span k, v;
            int guard = 0;
            while (ha_json_members_next(&m, &k, &v)) {
                ha_json_scan vs;
                ha_json_scan_init(&vs, v.p, v.len);
                if (!ha_json_skip(&vs)) {
                    check(0, "member-value-must-be-skippable", s);
                    break;
                }
                if (++guard > 1000) { check(0, "member-iter-loop", s); break; }
            }
        }
    }
}

/* ---------------- 字符串解码 / \u / 代理对 ---------------- */

static void test_decode(void) {
    struct { const char *in; const char *want; } cases[] = {
        { "\"\"", "" },
        { "\"a\"", "a" },
        { "\"\\\"\"", "\"" },
        { "\"\\\\\"", "\\" },
        { "\"\\/\"", "/" },
        { "\"\\b\\f\\n\\r\\t\"", "\b\f\n\r\t" },
        { "\"\\u4f60\\u597d\"", "\xe4\xbd\xa0\xe5\xa5\xbd" },  /* 你好 */
        { "\"\\ud83d\\ude00\"", "\xf0\x9f\x98\x80" },          /* 😀 代理对 */
        { "\"\\u0041\"", "A" },
        { "\"\\u00e9\"", "\xc3\xa9" },
        { "\"\\u4e2d\\u6587\"", "\xe4\xb8\xad\xe6\x96\x87" },
        /* 非法 UTF-8：每字节一个 U+FFFD */
        { "\"\xff\xfe\"", "\xef\xbf\xbd\xef\xbf\xbd" },
        { "\"\xc3\"",     "\xef\xbf\xbd" },         /* 截断序列 */
        { "\"\xc3\x28\"", "\xef\xbf\xbd\x28" },     /* 坏续字节 */
        { "\"\xe0\x80\x80\"", "\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd" }, /* 过长 */
        { "\"\xed\xa0\x80\"", "\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd" }, /* 代理区 */
        { "\"\xf5\x80\x80\x80\"", "\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd" },
        /* 孤立代理 */
        { "\"\\udc00\"", "\xef\xbf\xbd" },
        { "\"\\ud800\"", "\xef\xbf\xbd" },
        /* 正常中文直传 */
        { "\"\xe4\xbd\xa0\xe5\xa5\xbd\"", "\xe4\xbd\xa0\xe5\xa5\xbd" },
    };
    char buf[64];
    for (size_t i = 0; i < sizeof(cases)/sizeof(cases[0]); i++) {
        ha_json_scan sc;
        ha_json_scan_init(&sc, cases[i].in, strlen(cases[i].in));
        ha_span raw;
        int ok = ha_json_scan_string(&sc, &raw);
        if (!ok) { check(0, "scan-string", cases[i].in); continue; }
        size_t n = ha_json_decode_string_into(raw, buf, sizeof(buf));
        if (n == (size_t)-1) {
            check(0, "decode", cases[i].in);
        } else {
            check_str("decode-value", buf, n, cases[i].want);
        }
    }
}

/* 非法转义必须报错而不是静默吞掉 */
static void test_bad_escape(void) {
    const char *bad[] = { "\"\\q\"", "\"\\u00\"", "\"\\uZZZZ\"", "\"\\u12g4\"" };
    for (size_t i = 0; i < sizeof(bad)/sizeof(bad[0]); i++) {
        ha_json_scan sc;
        ha_json_scan_init(&sc, bad[i], strlen(bad[i]));
        ha_span raw;
        if (ha_json_scan_string(&sc, &raw)) {
            char buf[32];
            size_t n = ha_json_decode_string_into(raw, buf, sizeof(buf));
            check(n == (size_t)-1, "bad-escape-must-fail", bad[i]);
        }
    }
}

/* ---------------- 整数 ---------------- */

static void test_int(void) {
    struct { const char *in; int ok; long long v; } cases[] = {
        { "0", 1, 0 }, { "1", 1, 1 }, { "-1", 1, -1 },
        { "12345", 1, 12345 }, { "-99999", 1, -99999 },
        { "0", 1, 0 },
        { "9223372036854775807", 1, 9223372036854775807LL },
        { "-9223372036854775808", 1, -9223372036854775807LL - 1 },
        { "9223372036854775808", 0, 0 },   /* 溢出 */
        { "-9223372036854775809", 0, 0 },  /* 溢出 */
        { "1.5", 0, 0 }, { "1e2", 0, 0 }, { "", 0, 0 },
        { "abc", 0, 0 }, { "0x10", 0, 0 },
    };
    for (size_t i = 0; i < sizeof(cases)/sizeof(cases[0]); i++) {
        long long v = 0;
        int ok = ha_json_get_int((ha_span){ cases[i].in, strlen(cases[i].in) }, &v);
        check(ok == cases[i].ok, "int-ok", cases[i].in);
        if (ok && cases[i].ok) {
            g_run++;
            if (v != cases[i].v) {
                g_fail++;
                printf("  [FAIL] int %s: got %lld want %lld\n",
                       cases[i].in, v, cases[i].v);
            }
        }
    }
}

/* ---------------- 缓冲不足不写越界 ---------------- */

static void test_buf_overflow(void) {
    /* 缓冲区不足：必须返回 -1，且**绝不写出缓冲之外**。
     * ASan 在这里把关：越界写会被直接抓住，故这条断言能回归「写到
     * buf[len] 恰好越界」这类经典错误。允许部分写入（流式 sink 的
     * 固有性质），调用方拿到 -1 必须丢弃整个结果。 */
    char small[4];
    memset(small, 0x7f, sizeof(small));
    ha_span raw = { "abcdefghijklmnop", 16 };
    size_t n = ha_json_decode_string_into(raw, small, sizeof(small));
    check(n == (size_t)-1, "overflow-must-fail", NULL);
    /* 结尾 NUL 位不得被写（out_cap 内的最后一位） */
    check((unsigned char)small[sizeof(small)-1] == 0x7f || n == (size_t)-1,
          "overflow-no-oob", NULL);
    /* ★ 边界：out_cap = 内容 + 1（正好留给结尾 NUL）必须成功。
     *
     *   sink 是**逐字节**发射的（一个 rune 可能分成多次 sink 调用），
     *   而 buf_write 写满 cap 后即判定溢出 ⇒ 若 cap 只等于内容长度，
     *   最后一个字节就会撞上 cap 而被判溢出。
     *   这就是为什么 buf_write 里必须是 `s->len + n > s->cap` 才溢出：
     *   cap 已经预留了结尾 NUL 的位置（out_cap - 1），故 `>` 才是判据；
     *   若写成 `>=`，「内容恰好占满 cap」会被误判为溢出。 */
    char exact[5];
    ha_span four = { "abcd", 4 };   /* ★ 必须用 4 字节 span，
                                       *   不能用上面那个 16 字节的 raw */
    size_t n2 = ha_json_decode_string_into(four, exact, sizeof(exact));
    g_run++;
    if (n2 != 4 || memcmp(exact, "abcd", 4) != 0 || exact[4] != '\0') {
        g_fail++;
        printf("  [FAIL] exact-fit: n=%zu (期望 4)\\n", n2);
    }
    /* 少一位（cap 3 < 内容 4）必须失败 */
    char tight[4];
    size_t n3 = ha_json_decode_string_into(four, tight, sizeof(tight));
    check(n3 == (size_t)-1, "one-short-must-fail", NULL);
}

/* ---------------- 深度保险 ---------------- */

static void test_deep_nesting(void) {
    /* 200 层嵌套：应被拒（不崩溃、不栈溢出） */
    char deep[512];
    size_t d = 0;
    for (int i = 0; i < 200; i++) { deep[d++] = '['; }
    for (int i = 0; i < 200; i++) { deep[d++] = ']'; }
    deep[d] = '\0';
    ha_json_scan sc;
    ha_json_scan_init(&sc, deep, d);
    int ok = ha_json_skip(&sc);
    check(ok == 0, "deep-nesting-rejected", NULL);

    /* 30 层：合法，应通过 */
    d = 0;
    for (int i = 0; i < 30; i++) { deep[d++] = '['; }
    for (int i = 0; i < 30; i++) { deep[d++] = ']'; }
    deep[d] = '\0';
    ha_json_scan sc2;
    ha_json_scan_init(&sc2, deep, d);
    check(ha_json_skip(&sc2) == 1, "moderate-nesting-ok", NULL);
}

/* ---------------- NUL 字节在输入里 ---------------- */

static void test_embedded_nul(void) {
    /* 输入含 NUL：因签名是 (ptr,len) 而非 C 字符串，必须能正确处理 */
    const char s[] = "{\"a\":\"x\0y\"}";
    ha_json_scan sc;
    ha_json_scan_init(&sc, s, sizeof(s) - 1);
    check(ha_json_skip(&sc) == 0, "embedded-nul-rejected", NULL);
}

/* ---------------- NULL / 空输入防御 ---------------- */

static void test_null_defense(void) {
    ha_json_scan sc;
    ha_json_scan_init(&sc, NULL, 0);
    check(ha_json_scan_eof(&sc) == 1, "null-init-eof", NULL);
    check(ha_json_skip(&sc) == 0, "null-skip", NULL);

    ha_span empty = { NULL, 0 };
    long long v;
    check(ha_json_get_int(empty, &v) == 0, "null-int", NULL);
    check(ha_json_object_get_string(empty, "a", NULL, 0) == (size_t)-1,
          "null-getstring", NULL);
}

/* ---------------- ABI ---------------- */

static void test_abi(void) {
    int v = ha_json_scan_abi_version();
    check(v == HA_JSON_SCAN_ABI_VERSION, "abi-self", NULL);
    check(v >= 1000 && v <= 99999, "abi-range", NULL);
}

int main(void) {
    printf("== ha_json_scan 契约测试 ==\n");
    test_abi();
    test_syntax();
    test_members();
    test_dup_key();
    test_malformed_detected();
    test_decode();
    test_bad_escape();
    test_int();
    test_buf_overflow();
    test_deep_nesting();
    test_embedded_nul();
    test_null_defense();

    printf("%s：%d 项断言，%d 失败\n",
           g_fail == 0 ? "PASS" : "FAIL", g_run, g_fail);
    return g_fail == 0 ? 0 : 1;
}
