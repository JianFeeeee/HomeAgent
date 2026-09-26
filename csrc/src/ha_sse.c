/*
 * ha_sse.c — LLM 流式协议（SSE 分块）结构导航辅助层
 *
 * 语义与理由见 include/ha_sse.h。本文件被 C 门禁全量覆盖
 * （告警 / ASan+UBSan / arm64 交叉编译 / libFuzzer），故**不放**在
 * Go 的 cgo 前言里 —— 前言里的 C 代码逃出全部检查。
 */

#include "ha_sse.h"

#include <string.h>

int ha_sse_abi_version(void) {
    return HA_SSE_ABI_VERSION;
}

int ha_sse_obj_find(const ha_span *obj, const char *key, size_t keylen,
                    ha_span *out, int *dup) {
    ha_json_members m;
    ha_span k, v;
    int hit = 0;

    if (obj == NULL || key == NULL || out == NULL || dup == NULL) {
        return 0;
    }
    *dup = 0;
    out->p = NULL;
    out->len = 0;
    if (keylen == 0) {
        return 0;
    }
    if (!ha_json_members_init(&m, obj->p, obj->len)) {
        return -1;
    }
    while (ha_json_members_next(&m, &k, &v)) {
        /* ★ 精确比较（不做大小写折叠）：与 Go 的 map key 语义一致。
         *   §5.4-1 实测 {"TEXT":"up"} 取不到 text。 */
        if (k.len == keylen && memcmp(k.p, key, keylen) == 0) {
            if (hit) {
                *dup = 1;   /* 重复键：调用方整体回退 Go */
            }
            hit = 1;
            *out = v;      /* 后者胜 */
        }
    }
    if (!ha_json_members_complete(&m)) {
        return -1;   /* 对象畸形 */
    }
    return hit;
}

/* 单字节 ASCII 小写折叠（非 ASCII 原样，与 Go 对 ASCII 字段名的行为一致）。 */
static unsigned char sse_lower(unsigned char c) {
    return (c >= 'A' && c <= 'Z') ? (unsigned char)(c + 32) : c;
}

int ha_sse_obj_find_ci(const ha_span *obj, const char *key, size_t keylen,
                       ha_span *out, int *dup) {
    ha_json_members m;
    ha_span k, v;
    int hit = 0;

    if (obj == NULL || key == NULL || out == NULL || dup == NULL) {
        return 0;
    }
    *dup = 0;
    out->p = NULL;
    out->len = 0;
    if (keylen == 0) {
        return 0;
    }
    if (!ha_json_members_init(&m, obj->p, obj->len)) {
        return -1;
    }
    while (ha_json_members_next(&m, &k, &v)) {
        if (k.len == keylen) {
            size_t j = 0;
            while (j < keylen &&
                   sse_lower((unsigned char)k.p[j]) ==
                       sse_lower((unsigned char)key[j])) {
                j++;
            }
            if (j == keylen) {
                if (hit) {
                    *dup = 1;
                }
                hit = 1;
                *out = v;
            }
        }
    }
    if (!ha_json_members_complete(&m)) {
        return -1;
    }
    return hit;
}

int ha_sse_root_object(const ha_span *doc) {
    ha_json_scan sc;

    if (doc == NULL || doc->p == NULL || doc->len == 0) {
        return 0;
    }
    ha_json_scan_init(&sc, doc->p, doc->len);
    (void)ha_json_scan_ws(&sc);
    if (ha_json_scan_eof(&sc) || sc.s[sc.i] != '{') {
        return 0;   /* 顶层非对象：Go 的 Unmarshal 进 struct 会失败 */
    }
    if (!ha_json_skip(&sc)) {
        return 0;
    }
    /* 尾部只允许空白 —— 复刻 json.Unmarshal 对 trailing garbage 的拒绝 */
    (void)ha_json_scan_ws(&sc);
    return ha_json_scan_eof(&sc) ? 1 : 0;
}

int ha_sse_arr_first(const ha_span *arr, ha_span *out) {
    ha_json_scan sc;
    size_t start;

    if (arr == NULL || out == NULL) {
        return 0;
    }
    out->p = NULL;
    out->len = 0;
    if (arr->p == NULL || arr->len == 0) {
        return 0;
    }
    /* ★ 游标的 base 始终是 arr->p，中途只推进 i。
     *
     *   初版在这里犯过一个「重新 init 到 sc.s + sc.i」的错：那样 base 变了，
     *   随后的 start = sc.i 变成 0，out->p = arr->p + 0 ⇒ **返回的是数组本身**
     *   而不是第一个元素。症状是上层的 fastChoice 拿到 firstByte=='[' 直接回退，
     *   表现为「快速路径永远不生效」——
     *   而如果只看「结果与 Go 一致」，这个 bug 会**完全隐形**（回退总是正确）。
     *
     *   ★ 这正是「优化是否真的生效」必须单独断言的原因：
     *     等价性测试无法发现「一直回退」。
     */
    ha_json_scan_init(&sc, arr->p, arr->len);
    (void)ha_json_scan_ws(&sc);
    if (ha_json_scan_eof(&sc) || sc.s[sc.i] != '[') {
        return -1;
    }
    sc.i++;   /* 跳过 '[' */
    (void)ha_json_scan_ws(&sc);
    if (ha_json_scan_eof(&sc) || sc.s[sc.i] == ']') {
        return 0;   /* 空数组 */
    }
    start = sc.i;
    if (!ha_json_skip(&sc)) {
        return -1;
    }
    out->p = arr->p + start;
    out->len = sc.i - start;
    return 1;
}

int ha_sse_stringify(const ha_span *val, char *out, size_t cap, size_t *outlen) {
    size_t len = 0;
    ha_json_scan sc;
    ha_span raw;

    if (val == NULL || out == NULL || outlen == NULL ||
        val->p == NULL || val->len == 0) {
        return 0;
    }
    *outlen = 0;
    /* 上界：每个输入字节最坏变 3 字节 U+FFFD。不足则交回 Go 走
     * json.Unmarshal（宁可慢也不截断）。 */
    if (cap < val->len * 3u + 4u) {
        return 0;
    }

    if (val->p[0] == '"') {
        ha_json_scan_init(&sc, val->p, val->len);
        if (!ha_json_scan_string(&sc, &raw)) {
            return 0;
        }
        {
            size_t n = ha_json_decode_string_into(raw, out, cap);
            if (n == (size_t)-1) {
                return 0;
            }
            *outlen = n;
            return 1;
        }
    }

    if (val->p[0] == '[') {
        ha_json_scan_init(&sc, val->p, val->len);
        (void)ha_json_scan_ws(&sc);
        sc.i++;   /* 跳过 '[' */
        for (;;) {
            size_t start;
            ha_span elem;
            (void)ha_json_scan_ws(&sc);
            if (ha_json_scan_eof(&sc) || sc.s[sc.i] == ']') {
                break;
            }
            start = sc.i;
            if (!ha_json_skip(&sc)) {
                return 0;
            }
            elem.p = val->p + start;
            elem.len = sc.i - start;

            /* 只有对象元素才可能有 text（§5.4-2：其余静默跳过） */
            if (elem.len > 0 && elem.p[0] == '{') {
                ha_span txt;
                int dup = 0;
                int rc = ha_sse_obj_find(&elem, "text", 4, &txt, &dup);
                if (dup) {
                    return 0;   /* 重复 text 键 ⇒ 交回 Go（合并语义） */
                }
                /* 只有字符串形态的 text 才取（§5.4-3） */
                if (rc == 1 && txt.len > 0 && txt.p[0] == '"') {
                    ha_json_scan ts;
                    ha_span traw;
                    size_t n;
                    ha_json_scan_init(&ts, txt.p, txt.len);
                    if (!ha_json_scan_string(&ts, &traw)) {
                        return 0;
                    }
                    /* cap-len 已保证至少 1 字节可用（含结尾 NUL） */
                    n = ha_json_decode_string_into(traw, out + len, cap - len);
                    if (n == (size_t)-1) {
                        return 0;
                    }
                    len += n;
                }
            }
            (void)ha_json_scan_ws(&sc);
            if (ha_json_scan_eof(&sc)) {
                break;
            }
            if (sc.s[sc.i] == ',') {
                sc.i++;
                continue;
            }
            if (sc.s[sc.i] == ']') {
                break;
            }
            return 0;   /* 畸形数组 */
        }
        *outlen = len;
        return 1;
    }

    /* 对象 / 数字 / true / false / null ⇒ 需 json.Marshal 重新编码（§5.2） */
    return 0;
}

int ha_sse_arg_string(const ha_span *val, char *out, size_t cap, size_t *outlen) {
    ha_json_scan sc;
    ha_span raw;
    size_t n;

    if (val == NULL || out == NULL || outlen == NULL ||
        val->p == NULL || val->len == 0) {
        return 0;
    }
    *outlen = 0;
    if (val->p[0] != '"') {
        return 0;   /* 非字符串：交回 Go（需 json.Marshal 重新编码） */
    }
    if (cap < val->len * 3u + 4u) {
        return 0;
    }
    ha_json_scan_init(&sc, val->p, val->len);
    if (!ha_json_scan_string(&sc, &raw)) {
        return 0;
    }
    n = ha_json_decode_string_into(raw, out, cap);
    if (n == (size_t)-1) {
        return 0;
    }
    *outlen = n;
    return 1;
}
