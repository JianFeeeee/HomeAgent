#ifndef HA_REMOTEDEVICE_H
#define HA_REMOTEDEVICE_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ==================================================================
 *  ha_remotedevice — 远程设备接入 C SDK
 *
 *  零外部依赖，纯 C 实现，兼容嵌入式平台。
 *  传输层由用户实现（4 个函数指针），SDK 处理所有协议细节。
 *
 *  声明式设计:
 *    设备在代码中声明自己是什么(kind)和能做什么(caps)，
 *    声明支持哪些命令(shell/camerasue/screensee/...)并注册对应处理函数，
 *    SDK 自动处理协议握手、心跳、消息路由、结果回执。
 *
 *  协议流程:
 *    TCP 连接 → WS 升级 → hello(设备声明) → bind(令牌) → 就绪
 *    就绪后循环：读帧 → 按 handlers 表分发命令 → 自动回执结果
 * ================================================================== */

/* ======================== 状态码 ======================== */
typedef enum {
    HA_OK               = 0,
    HA_ERR_GENERIC      = -1,
    HA_ERR_NOMEM        = -2,
    HA_ERR_INVALID      = -3,
    HA_ERR_TIMEOUT      = -4,
    HA_ERR_DISCONNECTED = -5,
    HA_ERR_PROTOCOL     = -6,
    HA_ERR_TRANSPORT    = -7,
    HA_ERR_NOT_FOUND    = -8,
} ha_status_t;

/* ======================== 传输层抽象 ========================
 *
 * 用户必须实现这 4 个函数，适配不同平台（FreeRTOS+lwIP、Zephyr、裸机等）。
 *
 *  connect(ctx, host, port) → 建立 TCP 连接，返回 0 成功
 *  send(ctx, data, len)     → 发送 len 字节，返回实际发送字节数，-1 失败
 *  recv(ctx, buf, len)      → 接收最多 len 字节，返回实际接收字节数，0 断开，-1 失败
 *  close(ctx)               → 关闭连接
 */
typedef struct {
    int  (*connect)(void *ctx, const char *host, uint16_t port);
    int  (*send)(void *ctx, const uint8_t *data, int len);
    int  (*recv)(void *ctx, uint8_t *buf, int len);
    void (*close)(void *ctx);
    void *ctx;
} ha_transport_t;

/* ======================== 设备声明 ========================
 *
 * 声明式配置：设备在代码中声明自己的类型和能力。
 * 这些信息通过 hello 消息发送给网关。
 *
 *  device_id  — 唯一标识，如 "esp32-cam-1"
 *  name       — 设备显示名，如 "门口摄像头"
 *  kind       — 设备种类，如 "camera"、"computer"、"speaker"、"light"
 *  caps       — 能力数组，以 NULL 结尾，如 {"camera","status",NULL}
 *  info_json  — 额外信息（JSON 字符串），可选，如 '{"chip":"ESP32-S3","psram":8}'
 */
typedef struct {
    const char   *device_id;
    const char   *name;
    const char   *kind;
    const char  **caps;        /* NULL 结尾 */
    const char   *info_json;   /* 可选，NULL 或 JSON 字符串 */
} ha_device_info_t;

/* ======================== 命令结果 ========================
 *
 * 命令处理函数通过填写此结构体返回数据。
 * SDK 收到结果后自动发送回执（文本或二进制分块）。
 *
 * 使用方式：
 *   1. 简单文本：设置 status=0, output="结果文本"
 *   2. 二进制数据：设置 has_binary=1, binary_data/binary_len/mime
 *   3. 错误：设置 status=1, error="错误信息"
 *
 * 注意：output 字符串由 SDK 内部 strdup 后发送，handler 返回后即可释放。
 *       我们约定 handler 不负责分配，由 SDK 在内部做好拷贝。
 *       所以 handler 可以返回栈上或静态字符串。
 */
typedef struct {
    int         status;          /* 0=ok, 非0=error */
    const char *output;          /* 输出文本（如 base64 图像数据），SDK 内部拷贝 */
    const char *error;           /* 错误信息 */
    int         has_binary;      /* 1=通过二进制分块回传 */
    const char *binary_mime;     /* 二进制 MIME 类型 */
    const uint8_t *binary_data;  /* 二进制数据指针 */
    int         binary_len;      /* 二进制数据长度 */
} ha_cmd_result_t;

/* ======================== 命令处理声明 ========================
 *
 * 声明式命令注册：设备在配置中声明支持哪些命令，并绑定处理函数。
 *
 *  command 值说明：
 *   - "shell"               → 处理 shell 类型命令，args 为完整命令字符串
 *   - "camerasue"            → 处理 homeagent-camerasue 命令，args 为参数
 *   - "screensee"            → 处理 homeagent-screensee 命令
 *   - "speakeruse"           → 处理 homeagent-speakeruse 命令
 *   - "computeruse"          → 处理 homeagent-computeruse 命令
 *   - "clipboardsee"         → 处理 homeagent-clipboardsee 命令
 *   - "clipboardsue"         → 处理 homeagent-clipboardsue 命令
 *   - "screensue"            → 处理 homeagent-screensue 命令
 *   - "deviceinfo"           → 处理设备信息查询
 *   - 其他自定义命令名       → 按字符串匹配分发
 *
 * handler 处理完毕后只需填写 result 结构体，SDK 自动回执。
 */
typedef ha_status_t (*ha_cmd_handler_t)(const char *req_id, const char *args,
                                        ha_cmd_result_t *result, void *userdata);

typedef struct {
    const char        *command;    /* 命令名，如 "camerasue"、"shell" */
    ha_cmd_handler_t   handler;    /* 处理函数 */
} ha_cmd_handler_def_t;

/* 二进制数据接收回调：收到服务端推送的二进制数据（如 TTS 音频）时调用。
 *  data 指针在回调返回后失效，如需保存请拷贝。 */
typedef void (*ha_binary_handler_t)(const char *req_id, const char *kind,
                                    const char *mime, const uint8_t *data,
                                    int len, void *userdata);

/* 连接状态变化回调 */
typedef void (*ha_state_callback_t)(int connected, void *userdata);

/* ======================== 客户端配置 ========================
 *
 * 所有配置在 ha_client_new() 时一次性声明。
 * 声明式核心：handlers 表声明了设备支持的所有命令及其处理函数。
 */
typedef struct {
    ha_transport_t        transport;      /* 传输层实现（必须） */
    ha_device_info_t      device;         /* 设备声明（必须） */
    const char           *server;         /* 服务端地址，如 "192.168.1.100:9890"（必须） */
    const char           *token;          /* 接入令牌（必须） */

    ha_cmd_handler_def_t *handlers;       /* 声明式命令处理表，.command=NULL 标记结束 */
    ha_binary_handler_t   on_binary;      /* 二进制数据接收回调（可选） */
    ha_state_callback_t   on_state;       /* 状态变化回调（可选） */
    void                 *userdata;       /* 用户自定义数据，传给所有回调 */

    int                   ping_interval;  /* 心跳间隔秒数，0 则默认 30 */
    int                   max_reconnect;  /* 最大重连次数，-1 无限重连（默认），0 不重连 */
} ha_config_t;

/* ======================== 客户端 API ======================== */

typedef struct ha_client ha_client_t;

/* 创建客户端实例。config 数据会在内部拷贝，外部可释放。 */
ha_client_t *ha_client_new(const ha_config_t *config);

/* 启动连接：TCP 连接 → WS 升级 → hello → bind → 就绪。阻塞直到完成或失败。 */
ha_status_t  ha_client_start(ha_client_t *client);

/* 主循环处理：必须在用户的主循环中周期性调用。
 *   - 读取 WS 帧并分发
 *   - 按 handlers 表查找命令处理函数，自动回执结果
 *   - 处理心跳 ping/pong
 *   - 处理断线重连
 * 返回 HA_OK 表示正常，HA_ERR_DISCONNECTED 表示正在重连。 */
ha_status_t  ha_client_process(ha_client_t *client);

/* ===== 主动上报（设备主动推送，非命令响应） ===== */

/* 发送设备主动上报事件。type 如 "motion_detected"，detail 为 JSON 字符串。 */
void ha_client_send_event(ha_client_t *client, const char *type,
                          const char *detail);

/* 发送设备状态更新。status: "online"、"offline"、"busy" 等。 */
void ha_client_send_status(ha_client_t *client, const char *status);

/* ===== 生命周期 ===== */

/* 停止客户端，断开连接。 */
void ha_client_stop(ha_client_t *client);

/* 销毁客户端，释放所有资源。 */
void ha_client_destroy(ha_client_t *client);

/* ======================== 工具函数 ======================== */

/* 解析 homeagent-* 命令，返回能力名和参数。
 *   command = "camerasue 5"   → cap="camerasue",  args="5"
 *   command = "screensee"      → cap="screensee",  args=""
 *   command = "computeruse {...}" → cap="computeruse", args="..."  */
void ha_cmd_parse_homeagent(const char *command, const char **cap,
                            const char **args);

/* 解析 JSON 格式的命令参数，提取 action 和 JSON 字符串。
 *   command = "computeruse {\"action\":\"click\",\"x\":100}"
 *   → action="computeruse", json_str="{\"action\":\"click\",...}"  */
void ha_cmd_parse_json(const char *command, const char **action,
                       const char **json_str);

/* Base64 编码（用于将二进制数据编码为文本回传）。
 * 返回写入 out 的字节数（不含 \0），out 不足时返回所需长度。 */
int ha_base64_encode(const uint8_t *data, int len, char *out, int out_len);

/* 获取版本号 */
const char *ha_version(void);

#ifdef __cplusplus
}
#endif

#endif /* HA_REMOTEDEVICE_H */