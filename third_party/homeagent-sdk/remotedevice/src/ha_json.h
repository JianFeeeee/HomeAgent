#ifndef HA_JSON_H
#define HA_JSON_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ======================== JSON 解析器（DOM 风格） ======================== */
typedef enum {
    HA_JSON_NULL,
    HA_JSON_BOOL,
    HA_JSON_INT,
    HA_JSON_STRING,
    HA_JSON_ARRAY,
    HA_JSON_OBJECT,
} ha_json_type_t;

typedef struct ha_json_node {
    ha_json_type_t       type;
    union {
        int              bool_val;
        int              int_val;
        char            *str_val;
    };
    struct ha_json_node *next;     /* linked list for array/object items */
    struct ha_json_node *child;    /* first child for array/object */
    char               *key;       /* key for object members */
} ha_json_node_t;

/* 解析 JSON 字符串，返回根节点。失败返回 NULL。 */
ha_json_node_t *ha_json_parse(const char *str);

/* 从对象中按 key 获取字符串值，不存在返回 NULL */
const char     *ha_json_get_string(const ha_json_node_t *obj, const char *key);

/* 从对象中按 key 获取 int 值，不存在返回 def */
int             ha_json_get_int(const ha_json_node_t *obj, const char *key, int def);

/* 从对象中按 key 获取子节点，不存在返回 NULL */
ha_json_node_t *ha_json_get(const ha_json_node_t *obj, const char *key);

/* 获取数组长度 */
int             ha_json_array_len(const ha_json_node_t *arr);

/* 获取数组第 index 个元素，越界返回 NULL */
ha_json_node_t *ha_json_array_get(const ha_json_node_t *arr, int index);

/* 释放整个 JSON 树 */
void            ha_json_free(ha_json_node_t *root);

/* ======================== JSON 构建器（直接写缓冲区） ======================== */
typedef struct {
    char   *buf;
    int     len;
    int     cap;
    int     depth;
    int     item_count[16];  /* 每层已添加元素数，用于逗号判断 */
} ha_json_builder_t;

/* 初始化构建器 */
void ha_json_builder_init(ha_json_builder_t *jb, char *buf, int cap);

/* 清空构建器 */
void ha_json_builder_reset(ha_json_builder_t *jb);

/* 基础写入 */
void ha_json_builder_raw(ha_json_builder_t *jb, const char *s);

/* 逗号（自动判断是否需要加） */
void ha_json_builder_comma(ha_json_builder_t *jb);

/* 对象 */
void ha_json_builder_begin_object(ha_json_builder_t *jb);
void ha_json_builder_end_object(ha_json_builder_t *jb);

/* 数组 */
void ha_json_builder_begin_array(ha_json_builder_t *jb);
void ha_json_builder_end_array(ha_json_builder_t *jb);

/* 键名 */
void ha_json_builder_key(ha_json_builder_t *jb, const char *key);

/* 值 */
void ha_json_builder_add_string(ha_json_builder_t *jb, const char *val);
void ha_json_builder_add_int(ha_json_builder_t *jb, int val);
void ha_json_builder_add_bool(ha_json_builder_t *jb, int val);
void ha_json_builder_add_null(ha_json_builder_t *jb);

/* 快捷方法：直接写 "key":"val" */
void ha_json_builder_string(ha_json_builder_t *jb, const char *key, const char *val);
void ha_json_builder_int(ha_json_builder_t *jb, const char *key, int val);
void ha_json_builder_bool(ha_json_builder_t *jb, const char *key, int val);

/* 获取当前构建的字符串指针 */
const char *ha_json_builder_str(ha_json_builder_t *jb);

/* 获取当前长度 */
int         ha_json_builder_len(ha_json_builder_t *jb);

#ifdef __cplusplus
}
#endif

#endif /* HA_JSON_H */