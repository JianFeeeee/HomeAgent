#include "ha_ws.h"
#include <string.h>
#include <stdio.h>
#include <stdlib.h>

/* WS GUID 用于计算 Accept 值 */
#define WS_GUID "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

/* ======================== Base64 编码（用于 WS key） ======================== */
static const char b64t[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

static void base64_encode_bin(const uint8_t *in, int in_len, char *out) {
    int i = 0, j = 0;
    uint8_t b[3];
    while (i < in_len) {
        int rem = in_len - i;
        if (rem >= 3) {
            b[0] = in[i++]; b[1] = in[i++]; b[2] = in[i++];
            out[j++] = b64t[b[0] >> 2];
            out[j++] = b64t[((b[0] & 0x03) << 4) | (b[1] >> 4)];
            out[j++] = b64t[((b[1] & 0x0F) << 2) | (b[2] >> 6)];
            out[j++] = b64t[b[2] & 0x3F];
        } else if (rem == 2) {
            b[0] = in[i++]; b[1] = in[i++];
            out[j++] = b64t[b[0] >> 2];
            out[j++] = b64t[((b[0] & 0x03) << 4) | (b[1] >> 4)];
            out[j++] = b64t[(b[1] & 0x0F) << 2];
            out[j++] = '=';
        } else {
            b[0] = in[i++];
            out[j++] = b64t[b[0] >> 2];
            out[j++] = b64t[(b[0] & 0x03) << 4];
            out[j++] = '=';
            out[j++] = '=';
        }
    }
    out[j] = '\0';
}

/* 简单伪随机数生成器 */
static uint32_t ws_rand_state = 0;
static void ws_rand_seed(uint32_t seed) { ws_rand_state = seed; }
static uint32_t ws_rand(void) {
    ws_rand_state = ws_rand_state * 1103515245 + 12345;
    return ws_rand_state;
}

/* 生成 WS 握手 key */
static void ws_gen_key(char *out) {
    uint8_t buf[16];
    for (int i = 0; i < 16; i++) {
        buf[i] = (uint8_t)(ws_rand() & 0xFF);
    }
    base64_encode_bin(buf, 16, out);
}

/* ======================== 从传输层接收指定字节数 ======================== */
static int recv_all(ha_ws_t *ws, uint8_t *buf, int len) {
    int pos = 0;
    while (pos < len) {
        int n = ws->transport->recv(ws->transport->ctx, buf + pos, len - pos);
        if (n <= 0) return -1;
        pos += n;
    }
    return 0;
}

/* ======================== 发送 WS 帧 ======================== */
int ha_ws_send_frame(ha_ws_t *ws, int opcode, const uint8_t *payload, int len) {
    uint8_t hdr[14]; /* 最大帧头：2 + 8 + 4 = 14 */
    int hdr_len = 0;

    hdr[0] = 0x80 | opcode; /* FIN + opcode */
    hdr_len = 2;

    int ext_len = 0;
    if (len < 126) {
        hdr[1] = 0x80 | len; /* mask bit + length */
    } else if (len < 65536) {
        hdr[1] = 0x80 | 126;
        hdr_len = 4;
        hdr[2] = (uint8_t)(len >> 8);
        hdr[3] = (uint8_t)(len & 0xFF);
        ext_len = 2;
    } else {
        hdr[1] = 0x80 | 127;
        hdr_len = 10;
        uint64_t l = (uint64_t)len;
        for (int i = 8; i > 0; i--) {
            hdr[1 + i] = (uint8_t)(l & 0xFF);
            l >>= 8;
        }
        ext_len = 8;
    }

    /* mask key */
    uint8_t mask_key[4];
    mask_key[0] = (uint8_t)(ws_rand() & 0xFF);
    mask_key[1] = (uint8_t)(ws_rand() & 0xFF);
    mask_key[2] = (uint8_t)(ws_rand() & 0xFF);
    mask_key[3] = (uint8_t)(ws_rand() & 0xFF);

    int mask_off = 2 + ext_len;
    hdr[mask_off]     = mask_key[0];
    hdr[mask_off + 1] = mask_key[1];
    hdr[mask_off + 2] = mask_key[2];
    hdr[mask_off + 3] = mask_key[3];
    hdr_len = mask_off + 4;

    /* 发送帧头 */
    if (ws->transport->send(ws->transport->ctx, hdr, hdr_len) != hdr_len) {
        return -1;
    }

    /* 发送掩码后的 payload */
    if (len > 0) {
        /* 如果 payload 不大，用栈缓冲区 */
        uint8_t stack_buf[2048];
        uint8_t *masked = (len <= (int)sizeof(stack_buf)) ? stack_buf : (uint8_t *)malloc(len);
        if (!masked) return -1;

        for (int i = 0; i < len; i++) {
            masked[i] = payload[i] ^ mask_key[i & 3];
        }

        int ret = (ws->transport->send(ws->transport->ctx, masked, len) == len) ? 0 : -1;

        if (masked != stack_buf) free(masked);
        if (ret != 0) return -1;
    }

    return 0;
}

/* ======================== 公共 API ======================== */

int ha_ws_connect(ha_ws_t *ws, ha_transport_t *transport,
                  const char *host, uint16_t port,
                  const char *path, const char *token) {
    memset(ws, 0, sizeof(ha_ws_t));
    ws->transport = transport;
    ws->connected = 0;

    strncpy(ws->host, host, sizeof(ws->host) - 1);
    ws->port = port;
    strncpy(ws->path, path, sizeof(ws->path) - 1);
    if (token) strncpy(ws->token, token, sizeof(ws->token) - 1);

    /* 种子 */
    ws_rand_seed((uint32_t)(uintptr_t)ws ^ (uint32_t)port);

    /* 1. TCP 连接 */
    if (transport->connect(transport->ctx, host, port) != 0) {
        return -1;
    }

    /* 2. 发送 WS 升级请求 */
    char key[32];
    ws_gen_key(key);

    char req[1024];
    int n = snprintf(req, sizeof(req),
        "GET %s HTTP/1.1\r\n"
        "Host: %s:%u\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        "Sec-WebSocket-Key: %s\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "\r\n",
        path, host, (unsigned)port, key);

    /* 如果 token 存在，加到路径参数中 */
    if (token && token[0]) {
        n = snprintf(req, sizeof(req),
            "GET %s?token=%s HTTP/1.1\r\n"
            "Host: %s:%u\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            "Sec-WebSocket-Key: %s\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            "\r\n",
            path, token, host, (unsigned)port, key);
    }

    if (transport->send(transport->ctx, (uint8_t *)req, n) != n) {
        transport->close(transport->ctx);
        return -1;
    }

    /* 3. 读取响应头（直到 \r\n\r\n） */
    char resp[1024];
    int resp_len = 0;
    int found = 0;
    while (resp_len < (int)sizeof(resp) - 1) {
        int n = transport->recv(transport->ctx, (uint8_t *)(resp + resp_len), 1);
        if (n <= 0) {
            transport->close(transport->ctx);
            return -1;
        }
        resp_len += n;
        resp[resp_len] = '\0';
        if (resp_len >= 4 && strcmp(resp + resp_len - 4, "\r\n\r\n") == 0) {
            found = 1;
            break;
        }
    }
    if (!found) {
        transport->close(transport->ctx);
        return -1;
    }

    /* 4. 检查状态码 101 */
    if (strstr(resp, " 101 ") == NULL) {
        transport->close(transport->ctx);
        return -1;
    }

    ws->connected = 1;
    return 0;
}

int ha_ws_send_text(ha_ws_t *ws, const char *text) {
    if (!ws->connected) return -1;
    return ha_ws_send_frame(ws, WS_OPCODE_TEXT, (const uint8_t *)text, (int)strlen(text));
}

int ha_ws_send_binary(ha_ws_t *ws, const uint8_t *data, int len) {
    if (!ws->connected) return -1;
    return ha_ws_send_frame(ws, WS_OPCODE_BINARY, data, len);
}

int ha_ws_send_ping(ha_ws_t *ws) {
    if (!ws->connected) return -1;
    return ha_ws_send_frame(ws, WS_OPCODE_PING, NULL, 0);
}

int ha_ws_read_frame(ha_ws_t *ws, const uint8_t **payload, int *len) {
    if (!ws->connected) return -1;

    *payload = NULL;
    *len = 0;

    /* 读取帧头：2 字节 */
    uint8_t hdr[2];
    if (recv_all(ws, hdr, 2) != 0) {
        ws->connected = 0;
        return -1;
    }

    int opcode = hdr[0] & 0x0F;
    int masked = (hdr[1] & 0x80) ? 1 : 0;
    uint64_t frame_len = hdr[1] & 0x7F;

    if (frame_len == 126) {
        uint8_t ext[2];
        if (recv_all(ws, ext, 2) != 0) { ws->connected = 0; return -1; }
        frame_len = ((uint64_t)ext[0] << 8) | ext[1];
    } else if (frame_len == 127) {
        uint8_t ext[8];
        if (recv_all(ws, ext, 8) != 0) { ws->connected = 0; return -1; }
        frame_len = 0;
        for (int i = 0; i < 8; i++) {
            frame_len = (frame_len << 8) | ext[i];
        }
    }

    /* 读取 mask key */
    uint8_t mask_key[4] = {0, 0, 0, 0};
    if (masked) {
        if (recv_all(ws, mask_key, 4) != 0) { ws->connected = 0; return -1; }
    }

    /* 限制帧大小 */
    if (frame_len > sizeof(ws->read_buf)) {
        /* 帧太大，跳过 payload */
        uint64_t skip = frame_len;
        uint8_t tmp[256];
        while (skip > 0) {
            int to_skip = (skip > sizeof(tmp)) ? (int)sizeof(tmp) : (int)skip;
            if (recv_all(ws, tmp, to_skip) != 0) { ws->connected = 0; return -1; }
            skip -= to_skip;
        }
        return -1; /* 返回错误，帧太大 */
    }

    /* 读取 payload */
    if (frame_len > 0) {
        if (recv_all(ws, ws->read_buf, (int)frame_len) != 0) {
            ws->connected = 0;
            return -1;
        }
        /* 如果有 mask，解掩码 */
        if (masked) {
            for (uint64_t i = 0; i < frame_len; i++) {
                ws->read_buf[i] ^= mask_key[i & 3];
            }
        }
    }

    *payload = ws->read_buf;
    *len = (int)frame_len;

    switch (opcode) {
        case WS_OPCODE_CLOSE:
            ws->connected = 0;
            return WS_OPCODE_CLOSE;
        case WS_OPCODE_PING:
            return WS_OPCODE_PING;
        case WS_OPCODE_PONG:
            return WS_OPCODE_PONG;
        case WS_OPCODE_TEXT:
        case WS_OPCODE_BINARY:
            return opcode;
        default:
            return -1;
    }
}

void ha_ws_close(ha_ws_t *ws) {
    if (ws->connected) {
        ha_ws_send_frame(ws, WS_OPCODE_CLOSE, NULL, 0);
        ws->connected = 0;
    }
    ws->transport->close(ws->transport->ctx);
}