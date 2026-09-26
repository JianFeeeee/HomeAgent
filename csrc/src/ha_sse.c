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

/* ==================================================================== */
/* 批量定位：一次调用返回全部字段                                      */
/* ==================================================================== */

static int scan_first_and_count(const ha_span *arr, ha_span *first, int *count);
static int chunk_choice_dispatch(ha_span choice, ha_chunk_out *out);
static int chunk_delta_dispatch(ha_span delta, ha_chunk_out *out, int *dup);

static void slot_reset(ha_chunk_slot *s) {
    s->span.p = NULL;
    s->span.len = 0;
    s->kind = HA_CHUNK_KIND_ABSENT;
}

static void chunk_out_reset(ha_chunk_out *o) {
    size_t i;
    for (i = 0; i < (size_t)HA_CHUNK_SLOT_COUNT; i++) {
        slot_reset(&o->slot[i]);
    }
    o->content_off = 0;   o->content_len = 0;
    o->reasoning_off = 0; o->reasoning_len = 0;
    o->finish_off = 0;    o->finish_len = 0;
    o->choices_span.p = NULL;
    o->choices_span.len = 0;
    o->has_choices = 0;
    o->choices_kind = HA_CHUNK_KIND_ABSENT;
    o->choices_count = 0;
    o->choice0_kind = HA_CHUNK_KIND_ABSENT;
}

/* 键名比较：大小写不敏感（struct 字段语义）。
 * ★ 为什么不直接用 ha_json_key_eq：那个接收 ha_span，而这里要按
 *   已知长度比较（省掉 strlen）—— 且必须与 Go 对 struct 字段的匹配一致。 */
static int ci_eq(const char *p, const char *name, size_t n) {
    size_t i;
    for (i = 0; i < n; i++) {
        if (sse_lower((unsigned char)p[i]) != sse_lower((unsigned char)name[i])) {
            return 0;
        }
    }
    return 1;
}

static int kind_of(const ha_span *v) {
    if (v == NULL || v->p == NULL || v->len == 0) {
        return HA_CHUNK_KIND_ABSENT;
    }
    switch (v->p[0]) {
        case '"': return HA_CHUNK_KIND_STRING;
        case '{': return HA_CHUNK_KIND_OBJECT;
        case '[': return HA_CHUNK_KIND_ARRAY;
        case 'n': return HA_CHUNK_KIND_NULL;
        default:  return HA_CHUNK_KIND_OTHER;
    }
}

/* 把字符串值解码进 sbuf 的 [off,off+len)。返回 0 失败（空间不足/语法错）。 */
static int emit_decoded(ha_span val, char *sbuf, size_t scap, size_t *off,
                        size_t *outlen) {
    ha_json_scan sc;
    ha_span raw;
    size_t n;

    *off = 0;
    *outlen = 0;
    ha_json_scan_init(&sc, val.p, val.len);
    if (!ha_json_scan_string(&sc, &raw)) {
        return 0;
    }
    /* 上界检查：每个输入字节最坏变 3 字节 U+FFFD */
    if (scap < raw.len * 3u + 4u) {
        return 0;
    }
    n = ha_json_decode_string_into(raw, sbuf, scap);
    if (n == (size_t)-1) {
        return 0;
    }
    *off = 0;
    *outlen = n;
    return 1;
}

/*
 * 顶层单趟分派：遍历成员表一次，按名字分派到对应槽位。
 * 顶层键（choices/usage）是 **struct 字段** ⇒ 大小写不敏感。
 * 返回 0 = 正常（即使有重复键，dup 由调用方检查）；-1 = 畸形。
 */
static int chunk_top_dispatch(ha_span root, ha_chunk_out *out, int *dup) {
    ha_json_members m;
    ha_span k, v;
    ha_span el_tmp;
    int r;

    *dup = 0;
    if (!ha_json_members_init(&m, root.p, root.len)) {
        return -1;
    }
    while (ha_json_members_next(&m, &k, &v)) {
        int t = kind_of(&v);
        if (k.len == 7 && ci_eq(k.p, "choices", 7)) {
            if (out->choices_kind != HA_CHUNK_KIND_ABSENT) {
                *dup = 1;   /* 重复键：字段级合并语义 ⇒ 交回 Go */
            }
            out->has_choices = (t != HA_CHUNK_KIND_ABSENT &&
                                t != HA_CHUNK_KIND_NULL) ? 1 : 0;
            out->choices_kind = t;
            out->choices_span = v;
            if (t == HA_CHUNK_KIND_ARRAY) {
                /* 一趟同时得出「首元素 span」与「元素个数」。
                 * ★ 初版为了拿个数先把整个数组扫一遍、再调 ha_sse_arr_first
                 *   重新扫第二遍 —— 而单趟成员遍历实测 107ns，两趟就是白扔 100ns+。
                 */
                if (scan_first_and_count(&v, &el_tmp, &out->choices_count) != 0) {
                    return -1;
                }
                if (out->choices_count > 0) {
                    out->choice0_span = el_tmp;
                }
            }
            continue;
        }
        if (k.len == 5 && ci_eq(k.p, "usage", 5)) {
            if (out->slot[HA_CHUNK_SLOT_USAGE].kind != HA_CHUNK_KIND_ABSENT) {
                *dup = 1;
            }
            out->slot[HA_CHUNK_SLOT_USAGE].span = v;
            out->slot[HA_CHUNK_SLOT_USAGE].kind = t;
            continue;
        }
        /* 其余顶层键（id/object/created/model/system_fingerprint…）一律忽略。
         * ★ Go 侧 struct 未声明 ⇒ 忽略；没有「类型不符」的可能。 */
    }
    r = ha_json_members_complete(&m) ? 0 : -1;
    return r;
}

/* 一趟取数组的首元素 span 与元素个数。
 * 返回 0 成功；非 0 表示数组畸形。
 * ★ 超过 2 个元素即停止计数并置 *count = 2（调用方一律回退），
 *   这样超大数组不会白扫 —— 而 Go 侧那种输入压根不该走快速路径。 */
static int scan_first_and_count(const ha_span *arr, ha_span *first, int *count) {
    ha_json_scan sc;
    int n = 0;
    first->p = NULL;
    first->len = 0;
    ha_json_scan_init(&sc, arr->p, arr->len);
    (void)ha_json_scan_ws(&sc);
    if (ha_json_scan_eof(&sc) || sc.s[sc.i] != '[') {
        return -1;
    }
    sc.i++;
    for (;;) {
        size_t start;
        (void)ha_json_scan_ws(&sc);
        if (ha_json_scan_eof(&sc) || sc.s[sc.i] == ']') {
            break;
        }
        start = sc.i;
        if (!ha_json_skip(&sc)) {
            return -1;
        }
        if (n == 0) {
            first->p = arr->p + start;
            first->len = sc.i - start;
        }
        n++;
        if (n >= 2) {
            /* 已知 >1：调用方必然回退，无需继续扫 */
            *count = 2;
            return 0;
        }
        (void)ha_json_scan_ws(&sc);
        if (ha_json_scan_eof(&sc)) {
            return -1;
        }
        if (sc.s[sc.i] == ',') {
            sc.i++;
            continue;
        }
        if (sc.s[sc.i] == ']') {
            break;
        }
        return -1;
    }
    *count = n;
    return 0;
}

/* choice0 内的单趟分派：delta + finish_reason（struct 字段 ⇒ CI）。
 * 返回 0 正常；非 0 = 畸形或重复键。 */
static int chunk_choice_dispatch(ha_span choice, ha_chunk_out *out) {
    ha_json_members cm;
    ha_span ck, cv;

    if (!ha_json_members_init(&cm, choice.p, choice.len)) {
        return -1;
    }
    while (ha_json_members_next(&cm, &ck, &cv)) {
        int t = kind_of(&cv);
        if (ck.len == 5 && ci_eq(ck.p, "delta", 5)) {
            if (out->slot[HA_CHUNK_SLOT_DELTA].kind != HA_CHUNK_KIND_ABSENT) {
                return -1;   /* 重复键 */
            }
            out->slot[HA_CHUNK_SLOT_DELTA].span = cv;
            out->slot[HA_CHUNK_SLOT_DELTA].kind = t;
            continue;
        }
        if (ck.len == 13 && ci_eq(ck.p, "finish_reason", 13)) {
            if (out->slot[HA_CHUNK_SLOT_FINISH_REASON].kind != HA_CHUNK_KIND_ABSENT) {
                return -1;
            }
            out->slot[HA_CHUNK_SLOT_FINISH_REASON].span = cv;
            out->slot[HA_CHUNK_SLOT_FINISH_REASON].kind = t;
            continue;
        }
    }
    return ha_json_members_complete(&cm) ? 0 : -1;
}

/* delta 内单趟分派。delta 是 **struct** ⇒ 字段名大小写不敏感。 */
static int chunk_delta_dispatch(ha_span delta, ha_chunk_out *out, int *dup) {
    ha_json_members m;
    ha_span k, v;

    *dup = 0;
    if (!ha_json_members_init(&m, delta.p, delta.len)) {
        return -1;
    }
    while (ha_json_members_next(&m, &k, &v)) {
        int t = kind_of(&v);
        if (k.len == 7 && ci_eq(k.p, "content", 7)) {
            if (out->slot[HA_CHUNK_SLOT_CONTENT].kind != HA_CHUNK_KIND_ABSENT) {
                *dup = 1;
            }
            out->slot[HA_CHUNK_SLOT_CONTENT].span = v;
            out->slot[HA_CHUNK_SLOT_CONTENT].kind = t;
            continue;
        }
        if (k.len == 17 && ci_eq(k.p, "reasoning_content", 17)) {
            if (out->slot[HA_CHUNK_SLOT_REASONING].kind != HA_CHUNK_KIND_ABSENT) {
                *dup = 1;
            }
            out->slot[HA_CHUNK_SLOT_REASONING].span = v;
            out->slot[HA_CHUNK_SLOT_REASONING].kind = t;
            continue;
        }
        if (k.len == 10 && ci_eq(k.p, "tool_calls", 10)) {
            if (out->slot[HA_CHUNK_SLOT_TOOL_CALLS].kind != HA_CHUNK_KIND_ABSENT) {
                *dup = 1;
            }
            out->slot[HA_CHUNK_SLOT_TOOL_CALLS].span = v;
            out->slot[HA_CHUNK_SLOT_TOOL_CALLS].kind = t;
            continue;
        }
    }
    return ha_json_members_complete(&m) ? 0 : -1;
}

int ha_sse_chunk_locate(const char *data, size_t len, ha_chunk_out *out,
                        char *sbuf, size_t scap, size_t *sused) {
    ha_span root;
    int dup = 0;
    ha_span el, v;

    if (out == NULL || sused == NULL) {
        return HA_CHUNK_FALLBACK;
    }
    chunk_out_reset(out);
    *sused = 0;
    if (data == NULL || len == 0) {
        return HA_CHUNK_FALLBACK;
    }
    root.p = data;
    root.len = len;

    /* 顶层必须是「恰好一个」良构对象（含尾部残留检查） */
    if (!ha_sse_root_object(&root)) {
        return HA_CHUNK_FALLBACK;
    }

    if (chunk_top_dispatch(root, out, &dup) != 0) {
        return HA_CHUNK_FALLBACK;
    }
    if (dup) {
        return HA_CHUNK_FALLBACK;   /* §5.1 字段级合并 */
    }

    /* ---- choices[0] ---- */
    if (out->choices_kind == HA_CHUNK_KIND_ARRAY) {
        if (out->choices_count > 1) {
            /* Go 侧会解析**全部**元素；本层只认 [0]，其余元素可能类型不符
             * 而让 Go 整块作废 ⇒ 无法保证等价，必须回退。 */
            return HA_CHUNK_FALLBACK;
        }
        if (out->choices_count == 0) {
            out->choice0_kind = HA_CHUNK_KIND_ABSENT;
        } else {
            if (ha_sse_arr_first(&out->choices_span, &el) != 1) {
                return HA_CHUNK_FALLBACK;
            }
            out->choice0_kind = kind_of(&el);
            if (out->choice0_kind != HA_CHUNK_KIND_OBJECT) {
                /* Go 侧是 []struct：元素非对象 ⇒ 整块作废 */
                return HA_CHUNK_TYPE_FAIL;
            }
            /* 元素内的 delta / finish_reason（struct 字段 ⇒ CI），**一趟**取完。
             * ★ 初版这里对 choices 数组做了「数个数 + 取首元素」两趟、
             *   又在 choice0 内单独跑一趟 members —— 合计 3 趟。
             */
            {
                if (chunk_choice_dispatch(el, out) != 0) {
                    return HA_CHUNK_FALLBACK;
                }
            }
        }
    }

    /* ---- delta 内分派 ---- */
    if (out->slot[HA_CHUNK_SLOT_DELTA].kind == HA_CHUNK_KIND_OBJECT) {
        v = out->slot[HA_CHUNK_SLOT_DELTA].span;
        if (chunk_delta_dispatch(v, out, &dup) != 0) {
            return HA_CHUNK_FALLBACK;
        }
        if (dup) {
            return HA_CHUNK_FALLBACK;
        }
    } else if (out->slot[HA_CHUNK_SLOT_DELTA].kind == HA_CHUNK_KIND_OTHER) {
        /* delta 非对象：Go 侧 Unmarshal 到 struct 会失败 */
        return HA_CHUNK_TYPE_FAIL;
    }

    /* ---- content：字符串直接解码；文本数组走 stringify ---- */
    {
        ha_chunk_slot *cs = &out->slot[HA_CHUNK_SLOT_CONTENT];
        if (cs->kind == HA_CHUNK_KIND_STRING) {
            if (!emit_decoded(cs->span, sbuf, scap, &out->content_off,
                              &out->content_len)) {
                return HA_CHUNK_FALLBACK;
            }
            *sused = out->content_len;
        } else if (cs->kind == HA_CHUNK_KIND_ARRAY) {
            size_t n = 0;
            if (!ha_sse_stringify(&cs->span, sbuf, scap, &n)) {
                /* 需 json.Marshal 重新编码（§5.2）⇒ 交回 Go */
                return HA_CHUNK_FALLBACK;
            }
            out->content_off = 0;
            out->content_len = n;
            *sused = n;
        } else if (cs->kind == HA_CHUNK_KIND_OBJECT ||
                   cs->kind == HA_CHUNK_KIND_OTHER) {
            /* 对象/数字/布尔 ⇒ stringifyContent 走 json.Marshal（§5.2） */
            return HA_CHUNK_FALLBACK;
        }
        /* NULL / ABSENT ⇒ content=""（与 Go 的 stringifyContent(nil) 一致） */
    }

    /* ---- reasoning_content：Go 侧是 **string**（强类型） ----
     * 若是 string 则解码；若是 null/absent ⇒ ""；其它类型 ⇒ 整块作废。 */
    {
        ha_chunk_slot *rs = &out->slot[HA_CHUNK_SLOT_REASONING];
        if (rs->kind == HA_CHUNK_KIND_STRING) {
            if (!emit_decoded(rs->span, sbuf + *sused, scap - *sused,
                              &out->reasoning_off, &out->reasoning_len)) {
                return HA_CHUNK_FALLBACK;
            }
            out->reasoning_off += *sused;
            *sused += out->reasoning_len;
        } else if (rs->kind != HA_CHUNK_KIND_ABSENT &&
                   rs->kind != HA_CHUNK_KIND_NULL) {
            return HA_CHUNK_TYPE_FAIL;   /* 与 Go 的 Unmarshal 失败一致 */
        }
    }

    /* ---- finish_reason：Go 侧是 *string ---- */
    {
        ha_chunk_slot *fs = &out->slot[HA_CHUNK_SLOT_FINISH_REASON];
        if (fs->kind == HA_CHUNK_KIND_STRING) {
            if (!emit_decoded(fs->span, sbuf + *sused, scap - *sused,
                              &out->finish_off, &out->finish_len)) {
                return HA_CHUNK_FALLBACK;
            }
            out->finish_off += *sused;
            *sused += out->finish_len;
        } else if (fs->kind != HA_CHUNK_KIND_ABSENT &&
                   fs->kind != HA_CHUNK_KIND_NULL) {
            return HA_CHUNK_TYPE_FAIL;
        }
    }

    return HA_CHUNK_OK;
}
