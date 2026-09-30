/*
 * ha_codec.c — HomeAgent 内核编解码层（C 实现）
 *
 * ============================ 性能设计（勿回退）============================
 *   1. **不 malloc**：模型名折叠用栈缓冲（短名走快路径，超长走零分配的回退）。
 *   2. **不 strlen**：长度由调用方传入（见 ha_codec.h 签名说明）。
 *   3. **ASCII 批量快路径**：连续 ASCII 成批计数，避免逐字节函数调用。
 *   4. **截断提前短路**：数满 keep 个 rune 立即返回，不扫完整串。
 *   5. **截断返回字节数**而非字符串：结果必然是输入前缀，调用方自己切片。
 *
 * 初版的三个反例（实测代价，见 docs/zh/c-core/llm-orchestration-c.md §7.1）：
 *   - Go 侧 C.CString（malloc+拷贝）＋ C 侧 strlen，单这一项约 75ns，
 *     而 cgo 边界本身仅约 32ns —— 即 **82% 的开销是自找的**，不是 cgo 的成本。
 *     初版由此得出「C 比 Go 慢」的结论是错的。
 *   - 逐字节 utf8_next 函数调用 ⇒ 1KB ASCII 比纯 Go 慢 7 倍。
 *   - 1KB 中文要先扫完整串才判断是否截断。
 *
 * 语义必须与 Go 侧实现逐值一致，由黄金对照测试钉死（含畸形 UTF-8）。
 */

#include "ha_codec.h"

#include <stdint.h>
#include <string.h>

/* ---------------------------------------------------------------- */
/* 大小写不敏感的子串匹配                                            */
/* ---------------------------------------------------------------- */

/* 只折 ASCII 字母；非 ASCII 字节原样（与 Go strings.ToLower 对模型名的
 * 实际效果一致——模型名都是 ASCII，中文/日文字节不受 ToLower 影响）。 */
static unsigned char ascii_lower(unsigned char c) {
    return (c >= 'A' && c <= 'Z') ? (unsigned char)(c + 32) : c;
}

/* 已折叠缓冲（长度 hn）中是否含子串 sub（sub 必须已小写、ASCII）。
 * memcmp 版本：折叠一次后可向量化比较，是短名快路径。 */
static int contains(const char *m, size_t hn, const char *sub) {
    size_t m_len = strlen(sub);
    if (m_len == 0 || hn < m_len) {
        return 0;
    }
    size_t last = hn - m_len;
    for (size_t i = 0; i <= last; i++) {
        /* 首字节过滤掉绝大多数位置，避免无谓 memcmp */
        if (m[i] == sub[0] && memcmp(m + i, sub, m_len) == 0) {
            return 1;
        }
    }
    return 0;
}

/* 边比较边折叠：**任意长度**都正确，无需缓冲（超长模型名的回退路径）。
 * sub 中的 ASCII 字母按小写处理；非 ASCII 字节按字节精确比较
 * （因此可直接用于 "\xe9\x9b\xb6\xe4\xb8\x80" 这类多字节字面量）。 */
static int contains_ci(const char *h, size_t hn, const char *sub) {
    size_t m_len = strlen(sub);
    if (m_len == 0 || hn < m_len) {
        return 0;
    }
    size_t last = hn - m_len;
    for (size_t i = 0; i <= last; i++) {
        size_t j = 0;
        while (j < m_len &&
               ascii_lower((unsigned char)h[i + j]) == (unsigned char)sub[j]) {
            j++;
        }
        if (j == m_len) {
            return 1;
        }
    }
    return 0;
}

/* 模型名的不可变视图：能进栈缓冲就折叠，否则按原样（用 contains_ci 匹配）。 */
typedef struct {
    const char *p;
    size_t n;
    int folded;
} model_view;

/* 栈缓冲容量：模型名实测都是几十字节。超出则退化为不折叠 +
 * contains_ci —— 仍**零分配且语义正确**，只是少了 memcmp 的向量化优势。 */
#define HA_MODEL_STACK 256

static int mv_contains(const model_view *v, const char *sub) {
    return v->folded ? contains(v->p, v->n, sub) : contains_ci(v->p, v->n, sub);
}

/* ---------------------------------------------------------------- */
/* 模型上下文窗口推断                                                */
/* ---------------------------------------------------------------- */

int ha_codec_model_context_window(const char *model, size_t model_len) {
    if (model == NULL || model_len == 0) {
        return HA_CODEC_CONTEXT_WINDOW_UNKNOWN;
    }

    char stack[HA_MODEL_STACK];
    model_view v;
    if (model_len < HA_MODEL_STACK) {
        for (size_t i = 0; i < model_len; i++) {
            stack[i] = (char)ascii_lower((unsigned char)model[i]);
        }
        stack[model_len] = '\0';
        v.p = stack;
        v.n = model_len;
        v.folded = 1;
    } else {
        v.p = model;
        v.n = model_len;
        v.folded = 0;
    }

    /* 顺序与 Go 侧 switch 分支**严格一致**：先匹配到的分支胜出。
     * 这不是「随便一组 if」，顺序错了就会给出不同窗口
     * （例：gpt-4-turbo 必须先于裸 gpt-4 命中）。 */
    if (mv_contains(&v, "deepseek-v4") || mv_contains(&v, "deepseek-v3")) {
        return 1048576;
    }
    if (mv_contains(&v, "deepseek-r1") || mv_contains(&v, "deepseek-chat")) {
        return 65536;
    }
    if (mv_contains(&v, "gpt-4")) {
        if (mv_contains(&v, "turbo") || mv_contains(&v, "mini") || mv_contains(&v, "omni")) {
            return 128000;
        }
        return 8192;
    }
    if (mv_contains(&v, "gpt-3.5")) {
        return 16384;
    }
    if (mv_contains(&v, "claude-3.5") || mv_contains(&v, "claude-3")) {
        return 200000;
    }
    if (mv_contains(&v, "claude")) {
        return 100000;
    }
    if (mv_contains(&v, "gemini-1.5") || mv_contains(&v, "gemini-2")) {
        return 1048576;
    }
    if (mv_contains(&v, "gemini")) {
        return 32768;
    }
    if (mv_contains(&v, "qwen")) {
        return 131072;
    }
    if (mv_contains(&v, "glm") || mv_contains(&v, "chatglm")) {
        return 131072;
    }
    if (mv_contains(&v, "llama-3")) {
        return 8192;
    }
    if (mv_contains(&v, "llama-2")) {
        return 4096;
    }
    if (mv_contains(&v, "mistral") || mv_contains(&v, "mixtral")) {
        return 32768;
    }
    /* "yi-" 与 "零一"（UTF-8 字面量）——contains_ci 对字节精确比较，
     * 故中文部分不受折叠影响，与 Go 的 strings.Contains 一致。 */
    if (mv_contains(&v, "yi-") || mv_contains(&v, "\xe9\x9b\xb6\xe4\xb8\x80")) {
        return 200000;
    }
    if (mv_contains(&v, "moonshot") || mv_contains(&v, "kimi")) {
        return 131072;
    }

    return HA_CODEC_CONTEXT_WINDOW_UNKNOWN;
}

/* ---------------------------------------------------------------- */
/* UTF-8 解码（与 Go utf8.DecodeRuneInString 逐值等价）               */
/* ---------------------------------------------------------------- */

/* 返回 s[0] 起始字符的字节长度（1..4）。
 *
 * 必须与 Go 的 utf8.DecodeRuneInString 语义一致——**包括无效序列只前进
 * 1 字节**（Go 对无效/截断序列返回 RuneError 且 size=1），否则 rune 计数
 * 会与 Go 分叉。这正是黄金对照测试用畸形输入能抓到的地方。
 *
 * remaining 是当前可读字节数。 */
static inline size_t utf8_char_len(const char *s, size_t remaining) {
    unsigned char c0 = (unsigned char)s[0];

    if (c0 < 0x80) {
        return 1; /* ASCII */
    }
    if (c0 < 0xC2) {
        return 1; /* 0x80..0xC1：续字节或过长编码 → Go 判无效，size=1 */
    }

    if (c0 < 0xE0) { /* 2 字节：0xC2..0xDF */
        if (remaining < 2) {
            return 1;
        }
        if (((unsigned char)s[1] & 0xC0) != 0x80) {
            return 1;
        }
        return 2;
    }

    if (c0 < 0xF0) { /* 3 字节：0xE0..0xEF */
        if (remaining < 3) {
            return 1;
        }
        /* 用 (c & 0xC0) == 0x80 走单条 AND+CMP（而非两条范围比较），
         * 并用 & 而非 && 避免短路分支——这是 CJK 主路径，须最短。 */
        unsigned char c1 = (unsigned char)s[1];
        unsigned char c2 = (unsigned char)s[2];
        if (((c1 & 0xC0) == 0x80) & ((c2 & 0xC0) == 0x80)) {
            /* 常见情形：既非 0xE0（防过长编码）也非 0xED（防代理对） */
            if (c0 != 0xE0 && c0 != 0xED) {
                return 3;
            }
            if ((c0 == 0xE0 && c1 >= 0xA0) || (c0 == 0xED && c1 <= 0x9F)) {
                return 3;
            }
        }
        return 1;
    }

    if (c0 < 0xF5) { /* 4 字节：0xF0..0xF4 */
        if (remaining < 4) {
            return 1;
        }
        unsigned char c1 = (unsigned char)s[1];
        unsigned char c2 = (unsigned char)s[2];
        unsigned char c3 = (unsigned char)s[3];
        if (((c1 & 0xC0) == 0x80) & ((c2 & 0xC0) == 0x80) & ((c3 & 0xC0) == 0x80)) {
            if (c0 != 0xF0 && c0 != 0xF4) {
                return 4;
            }
            if ((c0 == 0xF0 && c1 >= 0x90) || (c0 == 0xF4 && c1 <= 0x8F)) {
                return 4;
            }
        }
        return 1;
    }

    return 1; /* 0xF5..0xFF：无效 */
}

/* ASCII 批量扫描：返回从 text[i] 起连续 ASCII 的字节数（扫到串尾）。
 *
 * ★ 字（word）级探测：一次读 8 字节，用单条掩码判断「8 字节是否全为 ASCII」。
 *   逐字节比较会让 1KB ASCII 明显慢于纯 Go（后者内部有 8 字节快路径）。
 *   实测：逐字节版 ascii_1k 约 2318ns（比 Go 慢 7×），改字级后大幅收敛。 */
#define HA_HIGH_BITS 0x8080808080808080ULL

static size_t ascii_run(const char *text, size_t i, size_t len) {
    size_t j = i;
    while (j + 8 <= len) {
        uint64_t v;
        memcpy(&v, text + j, 8); /* memcpy 让编译器按需生成未对齐安全加载 */
        if (v & HA_HIGH_BITS) {
            break;
        }
        j += 8;
    }
    while (j < len && (unsigned char)text[j] < 0x80) {
        j++;
    }
    return j - i;
}

/* ---------------------------------------------------------------- */
/* token 估算                                                        */
/* ---------------------------------------------------------------- */

int ha_codec_estimate_tokens(const char *text, size_t text_len) {
    if (text == NULL || text_len == 0) {
        return 0;
    }

    size_t runes = 0;
    size_t i = 0;
    while (i < text_len) {
        if ((unsigned char)text[i] < 0x80) {
            size_t n = ascii_run(text, i, text_len);
            runes += n;
            i += n;
        } else {
            i += utf8_char_len(text + i, text_len - i);
            runes++;
        }
    }

    /* 与 Go 侧 estimateTokensPure 一致：t = min(text_len, runes * 2)。
     * text_len 是**数学上界**（每个 token 至少覆盖 1 字节），
     * runes * 2 是实测校准（CJK/emoji）。详见 Go 侧注释与
     * internal/agent/api/codec_golden_test.go 的跨语言一致性判据。
     * runes > 0 时 runes*2 >= 2，故只需处理溢出与下限。 */
    if (runes > (size_t)0x3FFFFFFF) { /* 防 runes*2 窄化到 int 溢出 */
        return 0x7FFFFFFF;
    }
    size_t by_runes = runes * 2;
    size_t t = (text_len < by_runes) ? text_len : by_runes;
    if (t > (size_t)0x7FFFFFFF) { /* 极端长串：饱和到 int 上限 */
        return 0x7FFFFFFF;
    }
    if (t < 1) {
        return 1;
    }
    return t;
}

/* ---------------------------------------------------------------- */
/* 按 token 预算计算应保留的字节数                                    */
/* ---------------------------------------------------------------- */

size_t ha_codec_truncate_by_tokens(const char *text, size_t text_len,
                                   int max_tokens) {
    if (text == NULL || text_len == 0 || max_tokens <= 0) {
        return 0;
    }

    /* 要保留的 rune 数（与 Go 一致：整数除法）。keep==0 时循环首轮即返回 0。 */
    size_t keep = (size_t)(max_tokens / 2);

    /* 提前短路：keep 个 rune 数满而串仍有剩余 ⇒ 必然截断，直接返回该字节边界，
     * 不必扫完整串（长文本上的主要收益）。
     * 若数完整串仍未数满 keep ⇒ 未超预算，返回全长（= 不截断）。 */
    size_t runes = 0;
    size_t i = 0;
    while (i < text_len) {
        if (runes == keep) {
            return i;
        }
        if ((unsigned char)text[i] < 0x80) {
            size_t n = ascii_run(text, i, text_len);
            if (runes + n >= keep) {
                /* keep 落在这批 ASCII 内：批内每字节一个 rune */
                return i + (keep - runes);
            }
            runes += n;
            i += n;
        } else {
            runes++;
            i += utf8_char_len(text + i, text_len - i);
        }
    }
    return text_len; /* 未超预算：整串都留 */
}

/* ---------------------------------------------------------------- */
/* ABI 自述                                                          */
/* ---------------------------------------------------------------- */

int ha_codec_abi_version(void) {
    return HA_CODEC_ABI_VERSION;
}
