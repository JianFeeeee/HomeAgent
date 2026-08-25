#ifndef HA_WS_H
#define HA_WS_H

#include <stdint.h>
#include <stddef.h>
#include "../include/ha_remotedevice.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ======================== WS 帧类型 ======================== */
#define WS_OPCODE_CONTINUATION  0x0
#define WS_OPCODE_TEXT          0x1
#define WS_OPCODE_BINARY        0x2
#define WS_OPCODE_CLOSE         0x8
#define WS_OPCODE_PING          0x9
#define WS_OPCODE_PONG          0xA

/* ======================== WS 连接 ======================== */
typedef struct {
    ha_transport_t *transport;   /* 用户实现的传输层 */
    int             connected;   /* 是否已连接 */
    uint8_t         read_buf[8192];  /* 读缓冲区 */
    int             read_pos;    /* 缓冲区中有效数据起始位置 */
    int             read_len;    /* 缓冲区中有效数据长度 */
    char            host[256];   /* 缓存目标地址 */
    uint16_t        port;
    char            path[256];
    char            token[256];
} ha_ws_t;

/* 创建 WS 连接。返回 0 成功，非 0 失败。 */
int  ha_ws_connect(ha_ws_t *ws, ha_transport_t *transport,
                   const char *host, uint16_t port,
                   const char *path, const char *token);

/* 发送文本帧。返回 0 成功。 */
int  ha_ws_send_text(ha_ws_t *ws, const char *text);

/* 发送二进制帧。返回 0 成功。 */
int  ha_ws_send_binary(ha_ws_t *ws, const uint8_t *data, int len);

/* 发送 ping。返回 0 成功。 */
int  ha_ws_send_ping(ha_ws_t *ws);

/* 读取一帧。
 * 返回 opcode (0x1/0x2/0x8/0x9/0xA)，-1 表示关闭或错误。
 * payload 和 len 指向内部缓冲区，在下次调用前有效。 */
int  ha_ws_read_frame(ha_ws_t *ws, const uint8_t **payload, int *len);

/* 发送原始 WS 帧（内部使用，用于回复 ping） */
int  ha_ws_send_frame(ha_ws_t *ws, int opcode, const uint8_t *payload, int len);

/* 关闭 WS 连接 */
void ha_ws_close(ha_ws_t *ws);

#ifdef __cplusplus
}
#endif

#endif /* HA_WS_H */