/*
 * ha_json_scan.c — HomeAgent 内核 LLM 协议层 JSON 扫描/取值（C 实现）
 *
 * ============================ 性能设计（勿回退） ============================
 *   1. **不 malloc**：一切结果以 span 回传，Go 侧零拷贝切片
 *   2. **不 strlen**：长度由调用方传入
 *   3. **不预扫**：scan 只在需要时前进一步；「找键」靠 members 迭代单趟，
 *      不先扫一遍收集全部键（那会缓存踩踏 + 二次遍历）
 *   4. **整数不走 strtoll**：strtoll 要 NUL 结尾或处理 locale，
 *      自写定点解析只认 JSON 整数语法，顺带把溢出判掉
 *   5. **字符串不建索引**：不记录转义位置。需要时按需解码
 *
 * 参照第一刀的教训（docs/zh/c-core/llm-orchestration-c.md §7.1）：
 * 初版每次调用 C.CString（malloc+拷贝）＋ C 侧 strlen，单这两项就吃掉
 * 82% 的时间 —— 那不是 cgo 的固有成本，是自找的。本库从设计上排除这类开销。
 *
 * 语义必须与 Go `encoding/json` 一致，由黄金对照测试钉死
 * （真值表见 docs/zh/c-core/sse-codec-c.md §二）。
 */

#include "ha_json_scan.h"

#include <string.h>

/* ---------------------------------------------------------------- */
/* ABI 自述                                                          */
/* ---------------------------------------------------------------- */

int ha_json_scan_abi_version(void) {
    return HA_JSON_SCAN_ABI_VERSION;
}

/* ---------------------------------------------------------------- */
/* 基础工具                                                          */
/* ---------------------------------------------------------------- */

/* JSON 空白：Go 的 encoding/json 只认这四个（不是 isspace）。
 * 差一个字符就会与 Go 分叉，故显式列举而非用 ctype。 */
static int is_ws(unsigned char c) {
    return c == ' ' || c == '\t' || c == '\n' || c == '\r';
}

static unsigned char ascii_lower(unsigned char c) {
    return (c >= 'A' && c <= 'Z') ? (unsigned char)(c + 32) : c;
}

void ha_json_scan_init(ha_json_scan *sc, const char *s, size_t n) {
    if (sc == NULL) {
        return;
    }
    sc->s = (s != NULL) ? s : "";
    sc->n = (s != NULL) ? n : 0;
    sc->i = 0;
}

int ha_json_scan_ws(ha_json_scan *sc) {
    if (sc == NULL) {
        return 1;
    }
    while (sc->i < sc->n && is_ws((unsigned char)sc->s[sc->i])) {
        sc->i++;
    }
    return (sc->i < sc->n) ? 0 : 1;
}

int ha_json_scan_eof(const ha_json_scan *sc) {
    if (sc == NULL) {
        return 1;
    }
    return (sc->i >= sc->n) ? 1 : 0;
}

/* 当前字符；到结尾返回 '\0'（0）。调用方需先判 eof。 */
static char peek(const ha_json_scan *sc) {
    return (sc->i < sc->n) ? sc->s[sc->i] : '\0';
}

/* 前进一字节；越界时不动（保持 eof 语义稳定）。 */
static void bump(ha_json_scan *sc) {
    if (sc->i < sc->n) {
        sc->i++;
    }
}

static int expect(ha_json_scan *sc, char c) {
    if (ha_json_scan_ws(sc) || peek(sc) != c) {
        return 0;
    }
    bump(sc);
    return 1;
}

/* ---------------------------------------------------------------- */
/* 值扫描（skip 一个完整值）                                         */
/* ---------------------------------------------------------------- */

static int scan_value(ha_json_scan *sc, int depth);
static int hex_val(unsigned char c);

/* 扫描字符串（含引号），出原始内容 span。
 * depth 传入是因为 scan_value 会递归；字符串本身不递归但需要限额。 */
static int scan_string_raw(ha_json_scan *sc, ha_span *raw, int depth) {
    if (depth > 128) {
        return 0; /* 深度保险，正常文档远小于此 */
    }
    if (ha_json_scan_ws(sc) || peek(sc) != '"') {
        return 0;
    }
    bump(sc); /* 开引号 */
    size_t start = sc->i;
    while (sc->i < sc->n) {
        char c = sc->s[sc->i];
        if (c == '"') {
            if (raw != NULL) {
                raw->p = sc->s + start;
                raw->len = sc->i - start;
            }
            bump(sc); /* 闭引号 */
            return 1;
        }
        if (c == '\\') {
            bump(sc);
            if (sc->i >= sc->n) {
                return 0; /* 末尾悬空反斜杠 */
            }
            /* ★ 必须校验转义字符本身合法：Go 的 unquoteBytes 对未知转义
             *   （\q、\x、单独 \p）返回错误 ⇒ 整个 Unmarshal 失败。
             *   初版只 bump 不校验，于是 `{"a":"\q"}` 被 C 判为合法，
             *   而 json.Valid=false —— 黄金对照当场抓到。
             *   （`\u` 的 4 位十六进制在解码阶段校验：那是**值**层面的
             *     错误，与扫描阶段的「转义序列形状」是两回事。） */
            char e = sc->s[sc->i];
            if (e != '"' && e != '\\' && e != '/' && e != 'b' && e != 'f' &&
                e != 'n' && e != 'r' && e != 't' && e != 'u') {
                return 0;
            }
            /* ★ `\u` 必须紧跟 **4 位十六进制**，且这一校验属于**扫描**阶段：
             *   Go 的 json.Valid 会拒绝 `{"a":"\u00"}`（不足 4 位），
             *   而初版把它留到解码阶段 ⇒ scan 判合法、json.Valid 判非法，
             *   黄金对照当场抓到这条分叉。
             *   校验放在扫描阶段还有一个好处：畸形的 wire 数据在
             *   「找键」阶段就被拒，不必等到取值。 */
            if (e == 'u') {
                /* 用 size_t 递推偏移，避免 int 与 size_t 混算
                 * （-Wconversion/-Wsign-conversion 会拦下 sign-change）。 */
                if (sc->n - sc->i < 5u) {
                    return 0; /* 位数不足：还需 'u' 之后 4 位 */
                }
                for (size_t k = 1; k <= 4u; k++) {
                    if (hex_val((unsigned char)sc->s[sc->i + k]) < 0) {
                        return 0; /* 非十六进制 */
                    }
                }
            }
            bump(sc); /* 被转义的字符；\u 的 4 位十六进制由上面的循环覆盖 */
            continue;
        }
        if ((unsigned char)c < 0x20) {
            return 0; /* Go 拒绝字符串里的裸控制字符 */
        }
        bump(sc);
    }
    return 0; /* 未闭合 */
}

/* 扫描字面量：true / false / null。 */
static int scan_literal(ha_json_scan *sc) {
    static const char kTrue[] = "true";
    static const char kFalse[] = "false";
    static const char kNull[] = "null";
    size_t rest = sc->n - sc->i;
    const char *p = sc->s + sc->i;

    if (rest >= 4 && memcmp(p, kTrue, 4) == 0) {
        sc->i += 4;
        return 1;
    }
    if (rest >= 5 && memcmp(p, kFalse, 5) == 0) {
        sc->i += 5;
        return 1;
    }
    if (rest >= 4 && memcmp(p, kNull, 4) == 0) {
        sc->i += 4;
        return 1;
    }
    return 0;
}

/* 数字：只校验**语法**（不求值）。求值由 ha_json_get_int / 调用方负责。
 * 这与 Go 的分工一致：Go 在 unmarshal 时求值并做范围检查，
 * 而本层的取整数是独立的一步。 */
static int scan_number(ha_json_scan *sc) {
    size_t start = sc->i;
    if (sc->i < sc->n && peek(sc) == '-') {
        bump(sc);
    }
    /* 整数部分：0 或 [1-9][0-9]*（禁止前导零，与 Go 一致） */
    if (sc->i >= sc->n) {
        return 0;
    }
    if (peek(sc) == '0') {
        bump(sc);
    } else if (peek(sc) >= '1' && peek(sc) <= '9') {
        while (sc->i < sc->n && peek(sc) >= '0' && peek(sc) <= '9') {
            bump(sc);
        }
    } else {
        return 0;
    }
    /* 小数部分 */
    if (sc->i < sc->n && peek(sc) == '.') {
        bump(sc);
        if (sc->i >= sc->n || peek(sc) < '0' || peek(sc) > '9') {
            return 0; /* "1." 与 "1.e3" 非法 */
        }
        while (sc->i < sc->n && peek(sc) >= '0' && peek(sc) <= '9') {
            bump(sc);
        }
    }
    /* 指数部分 */
    if (sc->i < sc->n && (peek(sc) == 'e' || peek(sc) == 'E')) {
        bump(sc);
        if (sc->i < sc->n && (peek(sc) == '+' || peek(sc) == '-')) {
            bump(sc);
        }
        if (sc->i >= sc->n || peek(sc) < '0' || peek(sc) > '9') {
            return 0;
        }
        while (sc->i < sc->n && peek(sc) >= '0' && peek(sc) <= '9') {
            bump(sc);
        }
    }
    return (sc->i > start) ? 1 : 0;
}

/* 扫描数组/对象。用显式 depth 递归（不用堆栈，零分配）。 */
static int scan_container(ha_json_scan *sc, char open, char close, int depth) {
    if (!expect(sc, open)) {
        return 0;
    }
    if (ha_json_scan_ws(sc)) {
        return 0; /* 未闭合 */
    }
    if (peek(sc) == close) {
        bump(sc);
        return 1; /* 空容器 */
    }
    for (;;) {
        if (open == '{') {
            ha_span k;
            if (!scan_string_raw(sc, &k, depth + 1)) {
                return 0;
            }
            if (!expect(sc, ':')) {
                return 0;
            }
        }
        if (!scan_value(sc, depth + 1)) {
            return 0;
        }
        if (ha_json_scan_ws(sc)) {
            return 0;
        }
        if (peek(sc) == ',') {
            bump(sc);
            continue;
        }
        if (peek(sc) == close) {
            bump(sc);
            return 1;
        }
        return 0; /* 缺 '}' 或多余的 ',' 之后没有键 */
    }
}

static int scan_value(ha_json_scan *sc, int depth) {
    if (depth > 128) {
        return 0;
    }
    if (ha_json_scan_ws(sc)) {
        return 0;
    }
    char c = peek(sc);
    switch (c) {
        case '{': return scan_container(sc, '{', '}', depth);
        case '[': return scan_container(sc, '[', ']', depth);
        case '"': {
            ha_span tmp;
            return scan_string_raw(sc, &tmp, depth);
        }
        case 't': case 'f': case 'n': return scan_literal(sc);
        default:
            if (c == '-' || (c >= '0' && c <= '9')) {
                return scan_number(sc);
            }
            return 0;
    }
}

int ha_json_skip(ha_json_scan *sc) {
    if (sc == NULL) {
        return 0;
    }
    return scan_value(sc, 0);
}

int ha_json_scan_string(ha_json_scan *sc, ha_span *raw) {
    if (sc == NULL) {
        return 0;
    }
    return scan_string_raw(sc, raw, 0);
}

/* ---------------------------------------------------------------- */
/* 顶层对象成员迭代                                                  */
/* ---------------------------------------------------------------- */

int ha_json_members_init(ha_json_members *m, const char *s, size_t n) {
    if (m == NULL) {
        return 0;
    }
    ha_json_scan_init(&m->sc, s, n);
    m->started = 0;
    m->done = 0;
    m->error = 0;
    if (ha_json_scan_ws(&m->sc) || peek(&m->sc) != '{') {
        return 0;
    }
    bump(&m->sc);
    return 1;
}

int ha_json_members_next(ha_json_members *m, ha_span *key, ha_span *val) {
    if (m == NULL || m->done) {
        return 0;
    }
    if (ha_json_scan_ws(&m->sc)) {
        m->done = 1;
        m->error = 1;   /* 未闭合 */
        return 0;
    }
    if (peek(&m->sc) == '}') {
        bump(&m->sc);
        m->done = 1;
        m->error = 0;   /* 正常结束 */
        return 0;       /* 没有更多成员 */
    }
    /* ★ 不接受尾逗号：Go 的 decoder 在 ',' 之后要求必有下一个键。
     *   `{"a":1,}` 在 Go 侧是语法错误，故这里也必须拒绝。 */
    if (m->started) {
        if (peek(&m->sc) != ',') {
            m->done = 1;
            m->error = 1;
            return 0;
        }
        bump(&m->sc);
        if (ha_json_scan_ws(&m->sc)) {
            m->done = 1;
            m->error = 1;
            return 0;
        }
        if (peek(&m->sc) == '}') {
            m->done = 1;
            m->error = 1;   /* 尾逗号 */
            return 0;
        }
    }

    ha_span k;
    if (!scan_string_raw(&m->sc, &k, 0)) {
        m->done = 1;
        m->error = 1;
        return 0;
    }
    if (!expect(&m->sc, ':')) {
        m->done = 1;
        m->error = 1;
        return 0;
    }
    if (ha_json_scan_ws(&m->sc)) {
        m->done = 1;
        m->error = 1;
        return 0;
    }

    /* ★ 就地完整跳过一个值，得到它的精确 span。
     *   这样「返回 1」就蕴含「该成员良构」，且游标已推进到值之后。 */
    size_t vstart = m->sc.i;
    if (!scan_value(&m->sc, 0)) {
        m->done = 1;
        m->error = 1;
        return 0;
    }
    size_t vend = m->sc.i;

    m->started = 1;
    if (key != NULL) {
        *key = k;
    }
    if (val != NULL) {
        val->p = m->sc.s + vstart;
        val->len = vend - vstart;
    }
    return 1;
}

int ha_json_members_complete(const ha_json_members *m) {
    if (m == NULL) {
        return 0;
    }
    return (m->done && !m->error) ? 1 : 0;
}

int ha_json_key_eq(ha_span key, const char *name) {
    if (name == NULL) {
        return 0;
    }
    size_t nl = 0;
    while (name[nl] != '\0') {
        nl++;
    }
    if (key.len != nl) {
        return 0;
    }
    for (size_t i = 0; i < nl; i++) {
        if (ascii_lower((unsigned char)key.p[i]) !=
            ascii_lower((unsigned char)name[i])) {
            return 0;
        }
    }
    return 1;
}

/* ---------------------------------------------------------------- */
/* 字符串解码                                                        */
/* ---------------------------------------------------------------- */

/* U+FFFD 的 UTF-8 编码（Go 对非法字节的替换目标）。 */
static const char kReplacement[3] = { (char)0xEF, (char)0xBF, (char)0xBD };

/* 十六进制值；非十六进制返回 -1。 */
static int hex_val(unsigned char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

/* 把码点编码成 UTF-8 写给 sink。返回写入字节数。 */
static size_t emit_rune(unsigned long cp, ha_json_sink sink, void *ctx) {
    unsigned char buf[4];
    size_t len;
    if (cp < 0x80) {
        buf[0] = (unsigned char)cp;
        len = 1;
    } else if (cp < 0x800) {
        buf[0] = (unsigned char)(0xC0 | (cp >> 6));
        buf[1] = (unsigned char)(0x80 | (cp & 0x3F));
        len = 2;
    } else if (cp < 0x10000) {
        buf[0] = (unsigned char)(0xE0 | (cp >> 12));
        buf[1] = (unsigned char)(0x80 | ((cp >> 6) & 0x3F));
        buf[2] = (unsigned char)(0x80 | (cp & 0x3F));
        len = 3;
    } else {
        buf[0] = (unsigned char)(0xF0 | (cp >> 18));
        buf[1] = (unsigned char)(0x80 | ((cp >> 12) & 0x3F));
        buf[2] = (unsigned char)(0x80 | ((cp >> 6) & 0x3F));
        buf[3] = (unsigned char)(0x80 | (cp & 0x3F));
        len = 4;
    }
    sink(ctx, (const char *)buf, len);
    return len;
}

/* 解码一段 raw（已定位转义与续字节的边界）。
 *
 * 非法 UTF-8 语义必须与 Go 逐字节一致：
 *   Go 的 unquoteBytes 遇到非法序列时，把**能构成前缀的最长合法部分**先解出，
 *   再对**第一个坏字节**产出单个 U+FFFD，然后从坏字节**之后**继续。
 *   即：一个坏字节 = 一个 U+FFFD（不是整个序列变一个）。
 *   典型：`\xff\xfe` → 两个 U+FFFD（真值表 §2.8 实测确认）。
 */
static size_t decode_body(ha_span raw, ha_json_sink sink, void *ctx, int *err) {
    size_t out = 0;
    size_t i = 0;
    *err = 0;

    while (i < raw.len) {
        unsigned char c = (unsigned char)raw.p[i];

        /* --- 转义 --- */
        if (c == '\\') {
            if (i + 1 >= raw.len) {
                *err = 1;
                return out;
            }
            unsigned char e = (unsigned char)raw.p[i + 1];
            switch (e) {
                case '"':  sink(ctx, "\"", 1); out += 1; i += 2; continue;
                case '\\': sink(ctx, "\\", 1); out += 1; i += 2; continue;
                case '/':  sink(ctx, "/", 1);  out += 1; i += 2; continue;
                case 'b':  sink(ctx, "\b", 1); out += 1; i += 2; continue;
                case 'f':  sink(ctx, "\f", 1); out += 1; i += 2; continue;
                case 'n':  sink(ctx, "\n", 1); out += 1; i += 2; continue;
                case 'r':  sink(ctx, "\r", 1); out += 1; i += 2; continue;
                case 't':  sink(ctx, "\t", 1); out += 1; i += 2; continue;
                case 'u': {
                    /* 需要 4 位十六进制：i+2 .. i+5 */
                    if (i + 6 > raw.len) {
                        *err = 1;
                        return out;
                    }
                    int h0 = hex_val((unsigned char)raw.p[i + 2]);
                    int h1 = hex_val((unsigned char)raw.p[i + 3]);
                    int h2 = hex_val((unsigned char)raw.p[i + 4]);
                    int h3 = hex_val((unsigned char)raw.p[i + 5]);
                    if (h0 < 0 || h1 < 0 || h2 < 0 || h3 < 0) {
                        *err = 1;
                        return out;
                    }
                    unsigned long cp = (unsigned long)((h0 << 12) | (h1 << 8) |
                                                     (h2 << 4) | h3);
                    size_t adv = 6;
                    if (cp >= 0xD800 && cp <= 0xDBFF) {
                        /* 高代理：尝试与紧随的 \uDC00-\uDFFF 合成 4 字节 rune。
                         *
                         * ★ 必须用 combined 标志，而不是「合成成功就直接落到底部」：
                         *   本块末尾有一段**无条件的** replacement 发射（处理合成
                         *   失败的情形）。若成功的分支只设 cp/adv 而不跳过那一段，
                         *   会先把合成好的码点丢掉、再发一个 U+FFFD ——
                         *   实测症状：`\ud83d\ude00`（😀）得到 `\xef\xbf\xbd\xef\xbf\xbd`。
                         *   这个 bug 只有**真的代理对**才会触发（`\u4f60` 这类
                         *   非代理码点根本不进本块），是黄金对照最容易漏的一类。
                         *
                         * 下界必须是 'i + 6 < raw.len'（而非一次判 i+12 <= len）：
                         * 后者会连带拒绝「合法高代理位于字符串末尾」的正确输入。 */
                        int combined = 0;
                        if (i + 6 < raw.len && raw.p[i + 6] == '\\' &&
                            raw.p[i + 7] == 'u') {
                            int g0 = hex_val((unsigned char)raw.p[i + 8]);
                            int g1 = hex_val((unsigned char)raw.p[i + 9]);
                            int g2 = hex_val((unsigned char)raw.p[i + 10]);
                            int g3 = hex_val((unsigned char)raw.p[i + 11]);
                            if (g0 >= 0 && g1 >= 0 && g2 >= 0 && g3 >= 0) {
                                unsigned long lo = (unsigned long)(
                                    (g0 << 12) | (g1 << 8) | (g2 << 4) | g3);
                                if (lo >= 0xDC00 && lo <= 0xDFFF) {
                                    cp = 0x10000UL + ((cp - 0xD800UL) << 10) +
                                         (lo - 0xDC00UL);
                                    adv = 12;
                                    combined = 1;
                                }
                            }
                        }
                        if (!combined) {
                            /* 高代理后面不是合法低代理：发一个 U+FFFD，
                             * 只消费掉这个 6 字节 \uXXXX，让后面的内容按原样
                             * 继续解析（与 Go unquote 的行为一致）。 */
                            sink(ctx, kReplacement, 3);
                            out += 3;
                            i += adv;
                            continue;
                        }
                    }
                    if (cp >= 0xDC00 && cp <= 0xDFFF) {
                        /* 孤立低代理 → U+FFFD */
                        sink(ctx, kReplacement, 3);
                        out += 3;
                        i += 6;
                        continue;
                    }
                    out += emit_rune(cp, sink, ctx);
                    i += adv;
                    continue;
                }
                default:
                    /* Go 对未知转义（如 \q）报错 */
                    *err = 1;
                    return out;
            }
        }

        /* --- 普通字节 / 多字节序列 --- */
        if (c < 0x80) {
            char ch = (char)c;
            sink(ctx, &ch, 1);
            out += 1;
            i++;
            continue;
        }

        /* 尝试解析一个合法多字节序列。
         *
         * ★ 过长编码（overlong）检查**必须在续字节全部并入之后**做。
         *   初版把它写在这里、只用首字节的 cp：
         *       else if ((b0 & 0xF0) == 0xE0) { need = 3; cp = b0 & 0x0Fu; }
         *       if (need == 3 && cp < 0x800) valid = 0;   // ← 此时 cp 只有首字节的位
         *   而 0xE4 恰好满足 0x0F 掩码 ⇒ cp = 4 ⇒ 4 < 0x800 ⇒ 误判非法
         *   ⇒ 正常的「你」（e4 bd a0）被逐字节换成 6 个 U+FFFD（实测症状）。
         *   过长的真实判据是「完整码点 < 该长度的最小值」，
         *   即 0xC0/0x80、0xE0 0x80、0xF0 0x80/0x90 这几类前缀。 */
        size_t need;
        unsigned long cp;
        unsigned char b0 = c;
        if ((b0 & 0xE0) == 0xC0) { need = 2; cp = b0 & 0x1Fu; }
        else if ((b0 & 0xF0) == 0xE0) { need = 3; cp = b0 & 0x0Fu; }
        else if ((b0 & 0xF8) == 0xF0) { need = 4; cp = b0 & 0x07u; }
        else { need = 0; cp = 0; }

        int valid = (need != 0);
        if (valid) {
            for (size_t k = 1; k < need; k++) {
                if (i + k >= raw.len) { valid = 0; break; }
                unsigned char nb = (unsigned char)raw.p[i + k];
                if ((nb & 0xC0) != 0x80) { valid = 0; break; }
                cp = (cp << 6) | (unsigned long)(nb & 0x3F);
            }
        }
        if (valid) {
            /* 过长编码：按**完整码点**比该长度的最小合法值
             * （2B:0x80 / 3B:0x800 / 4B:0x10000） */
            if (need == 2 && cp < 0x80) valid = 0;
            if (need == 3 && cp < 0x800) valid = 0;
            if (need == 4 && cp < 0x10000) valid = 0;
            /* 代理区编码（CESU-8 / WTF-8）Go 判非法 */
            if (cp >= 0xD800 && cp <= 0xDFFF) valid = 0;
            if (cp > 0x10FFFF) valid = 0;
        }
        if (valid) {
            sink(ctx, raw.p + i, need);
            out += need;
            i += need;
            continue;
        }

        /* 非法：单个字节 → 一个 U+FFFD，然后继续（与 Go 逐字节一致） */
        sink(ctx, kReplacement, 3);
        out += 3;
        i++;
    }
    return out;
}

int ha_json_decode_string(ha_span raw, ha_json_sink sink, void *ctx,
                          size_t *out_len) {
    if (sink == NULL) {
        return 0;
    }
    int err = 0;
    size_t n = decode_body(raw, sink, ctx, &err);
    if (out_len != NULL) {
        *out_len = n;
    }
    return err ? 0 : 1;
}

/* ---------------------------------------------------------------- */
/* 写入缓冲的 sink                                                    */
/* ---------------------------------------------------------------- */

typedef struct {
    char  *out;
    size_t cap;
    size_t len;
} buf_sink;

static void buf_write(void *ctx, const char *b, size_t n) {
    buf_sink *s = (buf_sink *)ctx;
    /* 缓冲不足时，**绝不再往后写**，并标记溢出（len > cap 即可辨认）。
     *
     * ★ 契约是「不越界写」，不是「一个字节都不写」：本函数是流式的，
     *   写到这里才知道放不下，之前已写出的部分无法撤销。
     *   调用方拿到 (size_t)-1 时**必须丢弃整个结果**（Go 侧就是这么做的）。
     *   若真需要 all-or-nothing，调用方应先测得长度再分配（两趟）。
     *   这个取舍是有意的：单趟更快，而丢弃结果对调用方是廉价的。 */
    if (s->len + n > s->cap) {
        s->len = s->cap + 1; /* 标记溢出 */
        return;
    }
    memcpy(s->out + s->len, b, n);
    s->len += n;
}

size_t ha_json_decode_string_into(ha_span raw, char *out, size_t out_cap) {
    if (out == NULL || out_cap == 0) {
        return (size_t)-1;
    }
    buf_sink s;
    s.out = out;
    s.cap = out_cap - 1; /* 留一位给结尾 NUL */
    s.len = 0;
    int err = 0;
    (void)decode_body(raw, buf_write, &s, &err);
    if (err || s.len > s.cap) {
        return (size_t)-1;
    }
    out[s.len] = '\0';
    return s.len;
}

/* ---------------------------------------------------------------- */
/* 整数                                                              */
/* ---------------------------------------------------------------- */

int ha_json_get_int(ha_span raw, long long *out) {
    if (raw.len == 0 || out == NULL) {
        return 0;
    }
    size_t i = 0;
    int neg = 0;
    if (raw.p[0] == '-') {
        neg = 1;
        i = 1;
        if (raw.len == 1) {
            return 0;
        }
    }
    /* 只接受 **JSON 整数语法**：可选 '-' + (0 | [1-9][0-9]*)。
     * 小数点 / 指数一律判「不是整数」，由调用方按 Go 的
     * 「类型不匹配 ⇒ 整块作废」语义处理。
     *
     * ★ 必须禁前导零：JSON 里 `007` / `00` 是**非法数字**，
     *   而 strconv.ParseInt 会接受它。若这里跟着接受，
     *   就会出现「C 认得、json.Unmarshal 报错」的分叉 ——
     *   黄金对照当场抓到（实测分歧："007"、"00"）。
     *   本层的职责是回答「这是不是 JSON 整数」，不是「能不能转成数字」。 */
    if (raw.p[i] == '0' && raw.len - i > 1) {
        return 0; /* 前导零：00 / 01 / 007 均非法 */
    }
    for (size_t k = i; k < raw.len; k++) {
        if (raw.p[k] < '0' || raw.p[k] > '9') {
            return 0;
        }
    }
    unsigned long long acc = 0;
    const unsigned long long limit =
        neg ? 9223372036854775807ULL + 1ULL : 9223372036854775807ULL;
    for (size_t k = i; k < raw.len; k++) {
        unsigned d = (unsigned)(raw.p[k] - '0');
        if (acc > (limit - d) / 10ULL) {
            return 0; /* 溢出（与 Go 报错等价） */
        }
        acc = acc * 10ULL + d;
    }
    if (neg) {
        *out = (acc == 9223372036854775808ULL)
                   ? (-9223372036854775807LL - 1)
                   : -(long long)acc;
    } else {
        *out = (long long)acc;
    }
    return 1;
}

/* ---------------------------------------------------------------- */
/* 便捷取值                                                          */
/* ---------------------------------------------------------------- */

/* 在对象里定位键 name 的值 span。
 *
 * 找到返回 1 且 *val 覆盖该值的原始字节（未解码）；未找到 / 语法错返回 0。
 * 重复键取**最后一次**（与 Go 的后者胜一致）。
 *
 * 实现要点：members 迭代器只报「值的起始位置」，值本身由本函数用
 * ha_json_skip 消费并算出 span —— 这样两种便捷取值共用同一套定位逻辑，
 * 不会因各自实现而分叉。 */
static int find_value(ha_span obj, const char *name, ha_span *val) {
    ha_json_members m;
    if (!ha_json_members_init(&m, obj.p, obj.len)) {
        return 0;
    }
    ha_span key;
    ha_span v;
    int found = 0;
    ha_span last = { NULL, 0 };

    while (ha_json_members_next(&m, &key, &v)) {
        if (ha_json_key_eq(key, name)) {
            last = v;
            found = 1;   /* 重复键后者胜：继续扫，只保留最后一次 */
        }
    }
    /* ★ 严格性：畸形输入必须判「找不到键」——
     *   Go 侧语法错误会让 json.Unmarshal 失败、整块作废，
     *   若这里放宽成「扫到哪算哪」，就会比 Go 宽松（见真值表 §2.2）。 */
    if (!ha_json_members_complete(&m)) {
        return 0;
    }
    if (found && val != NULL) {
        *val = last;
    }
    return found;
}

size_t ha_json_object_get_string(ha_span obj, const char *name,
                                 char *out, size_t out_cap) {
    if (out == NULL || out_cap == 0) {
        return (size_t)-1;
    }
    ha_span val;
    if (!find_value(obj, name, &val)) {
        return (size_t)-1;
    }
    /* 只接受字符串值；其他类型视为「取不到」（类型判断由调用方按
     * Go 的 interface{}/强类型语义决定，见 sse-codec-c.md §2.2） */
    ha_json_scan sc;
    ha_json_scan_init(&sc, val.p, val.len);
    ha_span raw;
    if (!ha_json_scan_string(&sc, &raw)) {
        return (size_t)-1;
    }
    return ha_json_decode_string_into(raw, out, out_cap);
}

int ha_json_object_get_int(ha_span obj, const char *name, long long *out) {
    if (out == NULL) {
        return 0;
    }
    ha_span val;
    if (!find_value(obj, name, &val)) {
        return 0;
    }
    return ha_json_get_int(val, out);
}
