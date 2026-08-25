#include "ha_json.h"
#include <stdlib.h>
#include <string.h>
#include <ctype.h>
#include <stdio.h>

/* ======================== 解析器 ======================== */

/* 前向声明 */
static ha_json_node_t *parse_value(const char **pp);

/* 跳过空白 */
static const char *skip_ws(const char *p) {
    while (*p && (unsigned char)*p <= ' ') p++;
    return p;
}

/* 解析字符串（"..."），返回新分配的字符串，p 更新到结束引号后 */
static char *parse_string(const char **pp) {
    const char *p = skip_ws(*pp);
    if (*p != '"') return NULL;
    p++;
    int len = 0;
    const char *q = p;
    while (*q && *q != '"') {
        if (*q == '\\') { q++; if (*q) q++; }
        else q++;
        len++;
    }
    if (*q != '"') return NULL;
    char *s = (char *)malloc(len + 1);
    if (!s) return NULL;
    q = p;
    int i = 0;
    while (*q && *q != '"') {
        if (*q == '\\') {
            q++;
            switch (*q) {
                case '"':  s[i++] = '"'; break;
                case '\\': s[i++] = '\\'; break;
                case '/':  s[i++] = '/'; break;
                case 'b':  s[i++] = '\b'; break;
                case 'f':  s[i++] = '\f'; break;
                case 'n':  s[i++] = '\n'; break;
                case 'r':  s[i++] = '\r'; break;
                case 't':  s[i++] = '\t'; break;
                case 'u': q += 4; s[i++] = '?'; continue;
                default: s[i++] = *q; break;
            }
            q++;
        } else {
            s[i++] = *q++;
        }
    }
    s[i] = '\0';
    *pp = q + 1;
    return s;
}

static ha_json_node_t *new_node(ha_json_type_t type) {
    ha_json_node_t *n = (ha_json_node_t *)calloc(1, sizeof(ha_json_node_t));
    if (n) n->type = type;
    return n;
}

/* 解析数字 */
static ha_json_node_t *parse_number(const char **pp) {
    const char *p = *pp;
    int neg = 0;
    if (*p == '-') { neg = 1; p++; }
    if (!isdigit((unsigned char)*p)) return NULL;
    int val = 0;
    while (isdigit((unsigned char)*p)) {
        val = val * 10 + (*p - '0');
        p++;
    }
    if (*p == '.') { p++; while (isdigit((unsigned char)*p)) p++; }
    if (*p == 'e' || *p == 'E') {
        p++;
        if (*p == '+' || *p == '-') p++;
        while (isdigit((unsigned char)*p)) p++;
    }
    *pp = p;
    ha_json_node_t *n = new_node(HA_JSON_INT);
    if (n) n->int_val = neg ? -val : val;
    return n;
}

/* 解析 true/false/null */
static ha_json_node_t *parse_keyword(const char **pp) {
    const char *p = *pp;
    ha_json_node_t *n = NULL;
    if (strncmp(p, "true", 4) == 0 && !isalnum((unsigned char)p[4])) {
        n = new_node(HA_JSON_BOOL); if (n) n->bool_val = 1;
        *pp = p + 4;
    } else if (strncmp(p, "false", 5) == 0 && !isalnum((unsigned char)p[5])) {
        n = new_node(HA_JSON_BOOL); if (n) n->bool_val = 0;
        *pp = p + 5;
    } else if (strncmp(p, "null", 4) == 0 && !isalnum((unsigned char)p[4])) {
        n = new_node(HA_JSON_NULL);
        *pp = p + 4;
    }
    return n;
}

/* 解析对象 */
static ha_json_node_t *parse_object(const char **pp) {
    const char *p = skip_ws(*pp);
    if (*p != '{') return NULL;
    p++;
    ha_json_node_t *obj = new_node(HA_JSON_OBJECT);
    if (!obj) return NULL;
    ha_json_node_t **tail = &obj->child;
    p = skip_ws(p);
    if (*p == '}') { *pp = p + 1; return obj; }
    while (*p) {
        p = skip_ws(p);
        char *key = parse_string(&p);
        if (!key) break;
        p = skip_ws(p);
        if (*p != ':') { free(key); break; }
        p++;
        ha_json_node_t *val = parse_value(&p);
        if (!val) { free(key); break; }
        val->key = key;
        *tail = val;
        tail = &val->next;
        p = skip_ws(p);
        if (*p == ',') { p++; continue; }
        if (*p == '}') break;
    }
    p = skip_ws(p);
    if (*p == '}') { *pp = p + 1; return obj; }
    ha_json_free(obj);
    return NULL;
}

/* 解析数组 */
static ha_json_node_t *parse_array(const char **pp) {
    const char *p = skip_ws(*pp);
    if (*p != '[') return NULL;
    p++;
    ha_json_node_t *arr = new_node(HA_JSON_ARRAY);
    if (!arr) return NULL;
    ha_json_node_t **tail = &arr->child;
    p = skip_ws(p);
    if (*p == ']') { *pp = p + 1; return arr; }
    while (*p) {
        ha_json_node_t *val = parse_value(&p);
        if (!val) break;
        *tail = val;
        tail = &val->next;
        p = skip_ws(p);
        if (*p == ',') { p++; continue; }
        if (*p == ']') break;
    }
    p = skip_ws(p);
    if (*p == ']') { *pp = p + 1; return arr; }
    ha_json_free(arr);
    return NULL;
}

/* 解析值（主入口） */
static ha_json_node_t *parse_value(const char **pp) {
    const char *p = skip_ws(*pp);
    if (*p == '{') return parse_object(pp);
    if (*p == '[') return parse_array(pp);
    if (*p == '"') {
        char *s = parse_string(pp);
        if (!s) return NULL;
        ha_json_node_t *n = new_node(HA_JSON_STRING);
        if (!n) { free(s); return NULL; }
        n->str_val = s;
        return n;
    }
    if (*p == '-' || isdigit((unsigned char)*p)) return parse_number(pp);
    return parse_keyword(pp);
}

/* ======================== 公共 API ======================== */

ha_json_node_t *ha_json_parse(const char *str) {
    if (!str) return NULL;
    const char *p = str;
    return parse_value(&p);
}

const char *ha_json_get_string(const ha_json_node_t *obj, const char *key) {
    ha_json_node_t *n = ha_json_get(obj, key);
    if (!n || n->type != HA_JSON_STRING) return NULL;
    return n->str_val;
}

int ha_json_get_int(const ha_json_node_t *obj, const char *key, int def) {
    ha_json_node_t *n = ha_json_get(obj, key);
    if (!n || n->type != HA_JSON_INT) return def;
    return n->int_val;
}

ha_json_node_t *ha_json_get(const ha_json_node_t *obj, const char *key) {
    if (!obj || obj->type != HA_JSON_OBJECT) return NULL;
    ha_json_node_t *c = obj->child;
    while (c) {
        if (c->key && strcmp(c->key, key) == 0) return c;
        c = c->next;
    }
    return NULL;
}

int ha_json_array_len(const ha_json_node_t *arr) {
    if (!arr || arr->type != HA_JSON_ARRAY) return 0;
    int n = 0;
    ha_json_node_t *c = arr->child;
    while (c) { n++; c = c->next; }
    return n;
}

ha_json_node_t *ha_json_array_get(const ha_json_node_t *arr, int index) {
    if (!arr || arr->type != HA_JSON_ARRAY) return NULL;
    ha_json_node_t *c = arr->child;
    int i = 0;
    while (c) {
        if (i == index) return c;
        i++; c = c->next;
    }
    return NULL;
}

void ha_json_free(ha_json_node_t *root) {
    if (!root) return;
    ha_json_node_t *c = root->child;
    while (c) {
        ha_json_node_t *next = c->next;
        free(c->key);
        if (c->type == HA_JSON_STRING) free(c->str_val);
        ha_json_free(c);
        c = next;
    }
    free(root);
}

/* ======================== 构建器 ======================== */

static void json_escape(ha_json_builder_t *jb, const char *s) {
    if (!s) { ha_json_builder_raw(jb, "null"); return; }
    ha_json_builder_raw(jb, "\"");
    for (const char *p = s; *p; p++) {
        unsigned char c = (unsigned char)*p;
        switch (c) {
            case '"':  ha_json_builder_raw(jb, "\\\""); break;
            case '\\': ha_json_builder_raw(jb, "\\\\"); break;
            case '\b': ha_json_builder_raw(jb, "\\b"); break;
            case '\f': ha_json_builder_raw(jb, "\\f"); break;
            case '\n': ha_json_builder_raw(jb, "\\n"); break;
            case '\r': ha_json_builder_raw(jb, "\\r"); break;
            case '\t': ha_json_builder_raw(jb, "\\t"); break;
            default:
                if (c < 0x20) {
                    char buf[8];
                    snprintf(buf, sizeof(buf), "\\u%04x", c);
                    ha_json_builder_raw(jb, buf);
                } else {
                    char buf[2] = { (char)c, 0 };
                    ha_json_builder_raw(jb, buf);
                }
                break;
        }
    }
    ha_json_builder_raw(jb, "\"");
}

void ha_json_builder_init(ha_json_builder_t *jb, char *buf, int cap) {
    jb->buf = buf;
    jb->len = 0;
    jb->cap = cap;
    jb->depth = 0;
    if (cap > 0) buf[0] = '\0';
}

void ha_json_builder_reset(ha_json_builder_t *jb) {
    jb->len = 0;
    jb->depth = 0;
    if (jb->cap > 0) jb->buf[0] = '\0';
}

void ha_json_builder_raw(ha_json_builder_t *jb, const char *s) {
    while (*s && jb->len < jb->cap - 1) {
        jb->buf[jb->len++] = *s++;
    }
    jb->buf[jb->len] = '\0';
}

void ha_json_builder_comma(ha_json_builder_t *jb) {
    if (jb->depth > 0 && jb->item_count[jb->depth - 1] > 0) {
        ha_json_builder_raw(jb, ",");
    }
    if (jb->depth > 0) jb->item_count[jb->depth - 1]++;
}

void ha_json_builder_begin_object(ha_json_builder_t *jb) {
    ha_json_builder_comma(jb);
    ha_json_builder_raw(jb, "{");
    if (jb->depth < 16) jb->item_count[jb->depth] = 0;
    jb->depth++;
}

void ha_json_builder_end_object(ha_json_builder_t *jb) {
    jb->depth--;
    ha_json_builder_raw(jb, "}");
}

void ha_json_builder_begin_array(ha_json_builder_t *jb) {
    ha_json_builder_comma(jb);
    ha_json_builder_raw(jb, "[");
    if (jb->depth < 16) jb->item_count[jb->depth] = 0;
    jb->depth++;
}

void ha_json_builder_end_array(ha_json_builder_t *jb) {
    jb->depth--;
    ha_json_builder_raw(jb, "]");
}

void ha_json_builder_key(ha_json_builder_t *jb, const char *key) {
    ha_json_builder_comma(jb);
    json_escape(jb, key);
    ha_json_builder_raw(jb, ":");
}

void ha_json_builder_add_string(ha_json_builder_t *jb, const char *val) {
    json_escape(jb, val);
}

void ha_json_builder_add_int(ha_json_builder_t *jb, int val) {
    char buf[16];
    snprintf(buf, sizeof(buf), "%d", val);
    ha_json_builder_raw(jb, buf);
}

void ha_json_builder_add_bool(ha_json_builder_t *jb, int val) {
    ha_json_builder_raw(jb, val ? "true" : "false");
}

void ha_json_builder_add_null(ha_json_builder_t *jb) {
    ha_json_builder_raw(jb, "null");
}

void ha_json_builder_string(ha_json_builder_t *jb, const char *key, const char *val) {
    ha_json_builder_key(jb, key);
    json_escape(jb, val);
}

void ha_json_builder_int(ha_json_builder_t *jb, const char *key, int val) {
    ha_json_builder_key(jb, key);
    ha_json_builder_add_int(jb, val);
}

void ha_json_builder_bool(ha_json_builder_t *jb, const char *key, int val) {
    ha_json_builder_key(jb, key);
    ha_json_builder_add_bool(jb, val);
}

const char *ha_json_builder_str(ha_json_builder_t *jb) {
    return jb->buf;
}

int ha_json_builder_len(ha_json_builder_t *jb) {
    return jb->len;
}