#include "ha_remotedevice.h"
#include "ha_json.h"
#include "ha_ws.h"
#include <string.h>
#include <stdlib.h>
#include <stdio.h>

#define HA_VERSION "0.1.0"

/* 前向声明（因 handle_cmd_msg 需要调用这些函数，而它们定义在后面） */
void ha_client_send_result(ha_client_t *client, const char *req_id,
                           const char *status, const char *output,
                           const char *error);
void ha_client_send_data_chunked(ha_client_t *client, const char *req_id,
                                 const char *kind, const char *mime,
                                 const uint8_t *data, int len);

/* ======================== 内部状态 ======================== */
typedef enum {
    HA_STATE_INIT,
    HA_STATE_DISCONNECTED,
    HA_STATE_CONNECTING,
    HA_STATE_WS_UPGRADING,
    HA_STATE_HELLO_SENT,
    HA_STATE_BIND_SENT,
    HA_STATE_READY,
    HA_STATE_STOPPING,
} ha_state_t;

/* 语音数据聚合缓冲区 */
typedef struct {
    char     req_id[128];
    char     kind[64];
    char     mime[64];
    int      total;
    uint8_t *data;
    int      len;
    int      cap;
} ha_speech_accum_t;

struct ha_client {
    ha_config_t      config;        /* 拷贝的配置 */
    ha_state_t       state;
    int              reconnect_cnt; /* 当前重连次数 */
    ha_ws_t          ws;            /* WS 连接 */

    /* JSON 构建缓冲区 */
    char             json_buf[4096];
    ha_json_builder_t jb;

    /* 语音数据聚合 */
    ha_speech_accum_t speech;
};

/* ======================== 辅助函数 ======================== */

static void set_sockbuf(ha_client_t *c, int i) { (void)c; (void)i; }

/* Base64 编码表 */
static const char b64[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

int ha_base64_encode(const uint8_t *data, int len, char *out, int out_len) {
    int needed = ((len + 2) / 3) * 4 + 1;
    if (out_len < needed) {
        if (out_len > 0) out[0] = '\0';
        return needed;
    }
    int i = 0, j = 0;
    while (i < len) {
        int rem = len - i;
        uint8_t b0 = data[i++];
        uint8_t b1 = (rem > 1) ? data[i++] : 0;
        uint8_t b2 = (rem > 2) ? data[i++] : 0;
        out[j++] = b64[b0 >> 2];
        out[j++] = b64[((b0 & 0x03) << 4) | (b1 >> 4)];
        out[j++] = (rem > 1) ? b64[((b1 & 0x0F) << 2) | (b2 >> 6)] : '=';
        out[j++] = (rem > 2) ? b64[b2 & 0x3F] : '=';
    }
    out[j] = '\0';
    return j;
}

/* ======================== JSON 构建辅助 ======================== */
static void json_init(ha_client_t *c) {
    ha_json_builder_init(&c->jb, c->json_buf, sizeof(c->json_buf));
}

/* ======================== WS 发送 JSON ======================== */
static int ws_send_json(ha_client_t *c) {
    return ha_ws_send_text(&c->ws, c->json_buf);
}

/* ======================== 协议消息构造 ======================== */

/* 构建 hello 消息 */
static int send_hello(ha_client_t *c) {
    json_init(c);
    ha_json_builder_begin_object(&c->jb);
    ha_json_builder_string(&c->jb, "op", "hello");
    ha_json_builder_key(&c->jb, "device");
    ha_json_builder_begin_object(&c->jb);
    ha_json_builder_string(&c->jb, "device_id", c->config.device.device_id);
    ha_json_builder_string(&c->jb, "name", c->config.device.name);
    ha_json_builder_string(&c->jb, "kind", c->config.device.kind);
    /* caps */
    ha_json_builder_key(&c->jb, "caps");
    ha_json_builder_begin_array(&c->jb);
    if (c->config.device.caps) {
        for (const char **p = c->config.device.caps; *p; p++) {
            ha_json_builder_add_string(&c->jb, *p);
        }
    }
    ha_json_builder_end_array(&c->jb);
    /* info 可选 */
    if (c->config.device.info_json && c->config.device.info_json[0]) {
        ha_json_builder_string(&c->jb, "info", c->config.device.info_json);
    }
    ha_json_builder_end_object(&c->jb); /* device */
    ha_json_builder_end_object(&c->jb); /* root */
    return ws_send_json(c);
}

/* 构建 bind 消息 */
static int send_bind(ha_client_t *c) {
    json_init(c);
    ha_json_builder_begin_object(&c->jb);
    ha_json_builder_string(&c->jb, "op", "bind");
    ha_json_builder_string(&c->jb, "device_id", c->config.device.device_id);
    ha_json_builder_string(&c->jb, "token", c->config.token);
    ha_json_builder_end_object(&c->jb);
    return ws_send_json(c);
}

/* ======================== 消息处理 ======================== */

/* 在 handlers 表中查找命令处理函数 */
static ha_cmd_handler_def_t *find_handler(ha_client_t *c, const char *name) {
    if (!name || !c->config.handlers) return NULL;
    for (ha_cmd_handler_def_t *h = c->config.handlers; h->command; h++) {
        if (strcmp(h->command, name) == 0) return h;
    }
    return NULL;
}

/* 声明式命令分发：查找 handlers 表 → 调用 handler → 自动回执 */
static void handle_cmd_msg(ha_client_t *c, ha_json_node_t *msg) {
    const char *req_id = ha_json_get_string(msg, "req_id");
    const char *command = ha_json_get_string(msg, "command");
    const char *cmd_type = ha_json_get_string(msg, "cmd_type");
    if (!req_id || !command) return;
    if (!cmd_type) cmd_type = "homeagent";

    const char *handler_name = NULL;
    const char *args = command;

    if (strcmp(cmd_type, "shell") == 0) {
        handler_name = "shell";
        /* args 保持为完整命令字符串 */
    } else {
        /* homeagent-* 命令：提取能力名作为 handler 名 */
        const char *cap = command;
        const char *p = command;
        if (strncmp(p, "homeagent-", 10) == 0) p += 10;
        const char *space = strchr(p, ' ');
        if (space) {
            args = space + 1;
            /* handler_name 用静态缓冲区 */
            static char name_buf[128];
            int n = (int)(space - p);
            if (n > 127) n = 127;
            strncpy(name_buf, p, n);
            name_buf[n] = '\0';
            handler_name = name_buf;
        } else {
            handler_name = p;
            args = "";
        }
    }

    ha_cmd_handler_def_t *def = find_handler(c, handler_name);
    if (!def) {
        ha_client_send_result(c, req_id, "error", NULL,
                              "unsupported command");
        return;
    }

    /* 调用 handler，填写 result */
    ha_cmd_result_t result;
    memset(&result, 0, sizeof(result));
    ha_status_t st = def->handler(req_id, args, &result, c->config.userdata);

    /* 自动回执 */
    if (st != HA_OK) {
        ha_client_send_result(c, req_id, "error", NULL,
                              result.error ? result.error : "handler failed");
        return;
    }

    if (result.has_binary && result.binary_data && result.binary_len > 0) {
        /* 二进制分块回传 */
        ha_client_send_data_chunked(c, req_id,
            handler_name, result.binary_mime ? result.binary_mime : "application/octet-stream",
            result.binary_data, result.binary_len);
    } else {
        /* 文本回传 */
        ha_client_send_result(c, req_id, result.status == 0 ? "ok" : "error",
                              result.output, result.error);
    }
}

static void handle_speech_start(ha_client_t *c, ha_json_node_t *msg) {
    const char *req_id = ha_json_get_string(msg, "req_id");
    const char *kind = ha_json_get_string(msg, "kind");
    const char *mime = ha_json_get_string(msg, "mime");
    if (!req_id) return;

    /* 释放旧的聚合数据 */
    free(c->speech.data);
    memset(&c->speech, 0, sizeof(c->speech));

    strncpy(c->speech.req_id, req_id, sizeof(c->speech.req_id) - 1);
    if (kind) strncpy(c->speech.kind, kind, sizeof(c->speech.kind) - 1);
    if (mime) strncpy(c->speech.mime, mime, sizeof(c->speech.mime) - 1);
    c->speech.total = ha_json_get_int(msg, "total", 0);
}

static void handle_speech_end(ha_client_t *c, ha_json_node_t *msg) {
    const char *req_id = ha_json_get_string(msg, "req_id");
    if (!req_id || strcmp(req_id, c->speech.req_id) != 0) return;

    if (c->config.on_binary && c->speech.data && c->speech.len > 0) {
        c->config.on_binary(c->speech.req_id, c->speech.kind,
                            c->speech.mime, c->speech.data,
                            c->speech.len, c->config.userdata);
    }

    free(c->speech.data);
    memset(&c->speech, 0, sizeof(c->speech));
}

static void handle_text_message(ha_client_t *c, const uint8_t *payload, int len) {
    /* 解析 JSON */
    char *tmp = (char *)malloc(len + 1);
    if (!tmp) return;
    memcpy(tmp, payload, len);
    tmp[len] = '\0';

    ha_json_node_t *root = ha_json_parse(tmp);
    if (!root) { free(tmp); return; }

    const char *op = ha_json_get_string(root, "op");
    if (!op) { ha_json_free(root); free(tmp); return; }

    switch (c->state) {
        case HA_STATE_HELLO_SENT:
            if (strcmp(op, "hello_ack") == 0) {
                c->state = HA_STATE_BIND_SENT;
                send_bind(c);
            }
            break;
        case HA_STATE_BIND_SENT:
            if (strcmp(op, "bind_ack") == 0) {
                c->state = HA_STATE_READY;
                if (c->config.on_state) {
                    c->config.on_state(1, c->config.userdata);
                }
            }
            break;
        case HA_STATE_READY:
            if (strcmp(op, "cmd") == 0) {
                handle_cmd_msg(c, root);
            } else if (strcmp(op, "cmd_speech_start") == 0) {
                handle_speech_start(c, root);
            } else if (strcmp(op, "cmd_speech_end") == 0) {
                handle_speech_end(c, root);
            }
            break;
        default:
            break;
    }

    ha_json_free(root);
    free(tmp);
}

/* ======================== 连接管理 ======================== */

static int do_connect(ha_client_t *c) {
    c->state = HA_STATE_CONNECTING;
    c->reconnect_cnt++;

    /* 解析 server 地址 */
    char host[256] = {0};
    uint16_t port = 9890;
    const char *p = c->config.server;
    if (!p) return -1;

    /* 去掉 ws:// 前缀 */
    if (strncmp(p, "ws://", 5) == 0) p += 5;
    else if (strncmp(p, "wss://", 6) == 0) p += 6;

    /* 提取 host:port */
    const char *colon = strchr(p, ':');
    const char *slash = strchr(p, '/');
    if (colon && (!slash || colon < slash)) {
        int host_len = (int)(colon - p);
        if (host_len > (int)sizeof(host) - 1) host_len = sizeof(host) - 1;
        memcpy(host, p, host_len);
        host[host_len] = '\0';
        port = (uint16_t)atoi(colon + 1);
    } else {
        int host_len = (slash ? (int)(slash - p) : (int)strlen(p));
        if (host_len > (int)sizeof(host) - 1) host_len = sizeof(host) - 1;
        memcpy(host, p, host_len);
        host[host_len] = '\0';
    }

    c->state = HA_STATE_WS_UPGRADING;
    if (ha_ws_connect(&c->ws, &c->config.transport, host, port,
                      "/api/v1/device/ws", c->config.token) != 0) {
        c->state = HA_STATE_DISCONNECTED;
        return -1;
    }

    /* 发送 hello */
    c->state = HA_STATE_HELLO_SENT;
    if (send_hello(c) != 0) {
        ha_ws_close(&c->ws);
        c->state = HA_STATE_DISCONNECTED;
        return -1;
    }

    return 0;
}

/* ======================== 公共 API ======================== */

ha_client_t *ha_client_new(const ha_config_t *config) {
    ha_client_t *c = (ha_client_t *)calloc(1, sizeof(ha_client_t));
    if (!c) return NULL;
    memcpy(&c->config, config, sizeof(ha_config_t));
    c->state = HA_STATE_INIT;
    c->reconnect_cnt = 0;
    return c;
}

ha_status_t ha_client_start(ha_client_t *client) {
    if (!client) return HA_ERR_INVALID;
    if (client->state != HA_STATE_INIT) return HA_ERR_GENERIC;

    /* 默认心跳间隔 30 秒 */
    if (client->config.ping_interval <= 0) {
        client->config.ping_interval = 30;
    }

    if (do_connect(client) != 0) {
        return HA_ERR_TRANSPORT;
    }

    /* 等待 bind_ack（最多 5 秒） */
    int wait_ms = 5000;
    int step = 50;
    while (wait_ms > 0 && client->state != HA_STATE_READY) {
        /* 处理一帧 */
        ha_status_t st = ha_client_process(client);
        if (st != HA_OK && st != HA_ERR_DISCONNECTED) {
            return st;
        }
        if (client->state == HA_STATE_READY) return HA_OK;

        /* 简单延时：靠 process 中的 recv 阻塞 */
        wait_ms -= step;
    }

    return (client->state == HA_STATE_READY) ? HA_OK : HA_ERR_TIMEOUT;
}

ha_status_t ha_client_process(ha_client_t *client) {
    if (!client) return HA_ERR_INVALID;

    if (client->state == HA_STATE_STOPPING) {
        return HA_ERR_DISCONNECTED;
    }

    /* 断线重连 */
    if (client->state == HA_STATE_DISCONNECTED ||
        client->state == HA_STATE_INIT) {
        if (client->config.max_reconnect >= 0 &&
            client->reconnect_cnt > client->config.max_reconnect) {
            return HA_ERR_DISCONNECTED;
        }
        /* 非阻塞模式：不在这里阻塞等待重连，返回 HA_ERR_DISCONNECTED */
        return HA_ERR_DISCONNECTED;
    }

    if (!client->ws.connected) {
        client->state = HA_STATE_DISCONNECTED;
        if (client->config.on_state) {
            client->config.on_state(0, client->config.userdata);
        }
        return HA_ERR_DISCONNECTED;
    }

    /* 尝试读取一帧 */
    const uint8_t *payload = NULL;
    int len = 0;
    int ret = ha_ws_read_frame(&client->ws, &payload, &len);

    if (ret < 0) {
        /* 连接断开 */
        client->state = HA_STATE_DISCONNECTED;
        if (client->config.on_state) {
            client->config.on_state(0, client->config.userdata);
        }
        return HA_ERR_DISCONNECTED;
    }

    switch (ret) {
        case WS_OPCODE_TEXT:
            handle_text_message(client, payload, len);
            break;
        case WS_OPCODE_BINARY:
            /* 二进制帧：如果处于语音聚合状态，追加数据 */
            if (client->speech.req_id[0] && payload) {
                int new_len = client->speech.len + len;
                if (new_len > client->speech.cap) {
                    int new_cap = client->speech.cap ? client->speech.cap * 2 : 4096;
                    while (new_cap < new_len) new_cap *= 2;
                    uint8_t *nd = (uint8_t *)realloc(client->speech.data, new_cap);
                    if (!nd) break;
                    client->speech.data = nd;
                    client->speech.cap = new_cap;
                }
                memcpy(client->speech.data + client->speech.len, payload, len);
                client->speech.len = new_len;
            }
            break;
        case WS_OPCODE_PING:
            /* 回复 pong */
            ha_ws_send_frame(&client->ws, WS_OPCODE_PONG, NULL, 0);
            break;
        case WS_OPCODE_PONG:
            /* 收到 pong，忽略 */
            break;
        case WS_OPCODE_CLOSE:
            client->state = HA_STATE_DISCONNECTED;
            if (client->config.on_state) {
                client->config.on_state(0, client->config.userdata);
            }
            return HA_ERR_DISCONNECTED;
    }

    return HA_OK;
}

void ha_client_send_result(ha_client_t *client, const char *req_id,
                           const char *status, const char *output,
                           const char *error) {
    if (!client || client->state != HA_STATE_READY) return;
    json_init(client);
    ha_json_builder_begin_object(&client->jb);
    ha_json_builder_string(&client->jb, "op", "cmd_result");
    ha_json_builder_string(&client->jb, "req_id", req_id);
    ha_json_builder_string(&client->jb, "status", status ? status : "ok");
    ha_json_builder_string(&client->jb, "device_id", client->config.device.device_id);
    if (output && output[0]) {
        ha_json_builder_string(&client->jb, "output", output);
    }
    if (error && error[0]) {
        ha_json_builder_string(&client->jb, "error", error);
    }
    ha_json_builder_end_object(&client->jb);
    ws_send_json(client);
}

void ha_client_send_data_chunked(ha_client_t *client, const char *req_id,
                                 const char *kind, const char *mime,
                                 const uint8_t *data, int len) {
    if (!client || client->state != HA_STATE_READY) return;

    /* cmd_data_start */
    json_init(client);
    ha_json_builder_begin_object(&client->jb);
    ha_json_builder_string(&client->jb, "op", "cmd_data_start");
    ha_json_builder_string(&client->jb, "req_id", req_id);
    ha_json_builder_string(&client->jb, "kind", kind ? kind : "data");
    ha_json_builder_string(&client->jb, "mime", mime ? mime : "application/octet-stream");
    ha_json_builder_int(&client->jb, "total", len);
    ha_json_builder_int(&client->jb, "chunk_size", 8192);
    ha_json_builder_end_object(&client->jb);
    ws_send_json(client);

    /* 二进制帧分块发送 */
    int off = 0;
    while (off < len) {
        int chunk = len - off;
        if (chunk > 8192) chunk = 8192;
        if (ha_ws_send_binary(&client->ws, data + off, chunk) != 0) return;
        off += chunk;
    }

    /* cmd_data_end */
    json_init(client);
    ha_json_builder_begin_object(&client->jb);
    ha_json_builder_string(&client->jb, "op", "cmd_data_end");
    ha_json_builder_string(&client->jb, "req_id", req_id);
    ha_json_builder_string(&client->jb, "status", "ok");
    ha_json_builder_int(&client->jb, "total", len);
    ha_json_builder_end_object(&client->jb);
    ws_send_json(client);
}

void ha_client_send_event(ha_client_t *client, const char *type,
                          const char *detail) {
    if (!client || client->state != HA_STATE_READY) return;
    json_init(client);
    ha_json_builder_begin_object(&client->jb);
    ha_json_builder_string(&client->jb, "op", "event");
    ha_json_builder_string(&client->jb, "device_id", client->config.device.device_id);
    ha_json_builder_string(&client->jb, "type", type ? type : "");
    if (detail && detail[0]) {
        ha_json_builder_string(&client->jb, "payload", detail);
    }
    ha_json_builder_end_object(&client->jb);
    ws_send_json(client);
}

void ha_client_send_status(ha_client_t *client, const char *status) {
    if (!client || client->state != HA_STATE_READY) return;
    json_init(client);
    ha_json_builder_begin_object(&client->jb);
    ha_json_builder_string(&client->jb, "op", "status");
    ha_json_builder_string(&client->jb, "device_id", client->config.device.device_id);
    ha_json_builder_string(&client->jb, "status", status ? status : "online");
    ha_json_builder_end_object(&client->jb);
    ws_send_json(client);
}

void ha_client_stop(ha_client_t *client) {
    if (!client) return;
    client->state = HA_STATE_STOPPING;
    if (client->ws.connected) {
        ha_ws_close(&client->ws);
    }
}

void ha_client_destroy(ha_client_t *client) {
    if (!client) return;
    ha_client_stop(client);
    free(client->speech.data);
    free(client);
}

/* ======================== 工具函数 ======================== */

void ha_cmd_parse_homeagent(const char *command, const char **cap,
                            const char **args) {
    *cap = command;
    *args = "";

    if (!command) {
        *cap = "";
        return;
    }

    /* 去掉 homeagent- 前缀 */
    const char *p = command;
    if (strncmp(p, "homeagent-", 10) == 0) {
        p += 10;
    }

    /* 按空格分割 */
    const char *space = strchr(p, ' ');
    if (space) {
        /* cap 指向 p 但不包含空格，需要临时拷贝 */
        /* 返回指针到原始字符串，调用方用 strncpy 取出 */
        *cap = command; /* 调用方应使用 ha_cmd_parse_homeagent 的要小心 */
        /* 实际上，最简单的方式是原地修改，但 const 不允许 */
        /* 用静态缓冲区或让调用方自己处理 */
        static char cap_buf[256];
        int n = (int)(space - p);
        if (n > 255) n = 255;
        strncpy(cap_buf, p, n);
        cap_buf[n] = '\0';
        *cap = cap_buf;
        *args = space + 1;
    } else {
        static char cap_buf[256];
        strncpy(cap_buf, p, sizeof(cap_buf) - 1);
        cap_buf[sizeof(cap_buf) - 1] = '\0';
        *cap = cap_buf;
        *args = "";
    }
}

void ha_cmd_parse_json(const char *command, const char **action,
                       const char **json_str) {
    *action = "";
    *json_str = "";

    if (!command) return;

    const char *p = command;
    if (strncmp(p, "homeagent-", 10) == 0) {
        p += 10;
    }

    const char *brace = strchr(p, '{');
    if (brace) {
        static char act_buf[256];
        int n = (int)(brace - p);
        while (n > 0 && (p[n - 1] == ' ' || p[n - 1] == '\t')) n--;
        if (n > 255) n = 255;
        strncpy(act_buf, p, n);
        act_buf[n] = '\0';
        *action = act_buf;
        *json_str = brace;
    } else {
        static char act_buf[256];
        strncpy(act_buf, p, sizeof(act_buf) - 1);
        *action = act_buf;
    }
}

const char *ha_version(void) {
    return HA_VERSION;
}

