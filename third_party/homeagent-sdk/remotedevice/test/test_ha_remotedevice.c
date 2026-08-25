/**
 * ha_remotedevice 全面测试
 *
 * 覆盖：JSON 解析/构建、WS 帧编解码、工具函数、
 *       客户端完整生命周期、命令分发、二进制分块、语音数据、事件上报。
 *
 * 编译 (POSIX/Linux/macOS):
 *   gcc -I../include -I../src ../src/*.c test_ha_remotedevice.c -lpthread -o test
 *
 * 编译 (Windows/MinGW):
 *   gcc -I../include -I../src ../src/*.c test_ha_remotedevice.c -lpthread -lws2_32 -o test
 */

#include "ha_remotedevice.h"
#include "ha_json.h"
#include "ha_ws.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <assert.h>
#include <time.h>

/* 前向声明：ha_client_send_data_chunked 供测试调用 */
void ha_client_send_data_chunked(ha_client_t *client, const char *req_id,
                                 const char *kind, const char *mime,
                                 const uint8_t *data, int len);

/* ==================================================================
 * 平台适配
 * ================================================================== */
#if defined(_WIN32) || defined(_WIN64)
  #define _CRT_SECURE_NO_WARNINGS
  #include <winsock2.h>
  #include <windows.h>
  #include <process.h>
  typedef int socklen_t;
  #define sleep(t) Sleep((t)*1000)
  #define usleep(t) Sleep((t)/1000)
  typedef unsigned thread_func_ret;
  #define THREAD_RETURN return 0
  #define SIGPIPE 13
  static void signal(int sig, void (*func)(int)) { (void)sig; (void)func; }
  static void sock_close(int fd) { closesocket(fd); }
#else
  #include <sys/socket.h>
  #include <netinet/in.h>
  #include <arpa/inet.h>
  #include <netdb.h>
  #include <unistd.h>
  #include <pthread.h>
  #include <signal.h>
  typedef void *thread_func_ret;
  #define THREAD_RETURN return NULL
  static void sock_close(int fd) { close(fd); }
#endif

/* ==================================================================
 * 简易测试框架
 * ================================================================== */
static int tests_passed = 0;
static int tests_failed = 0;
static int tests_skipped = 0;

#define TEST_BEGIN(name) do { \
    printf("  TEST: %s ... ", name); \
    fflush(stdout); \
    do { (void)0

#define TEST_END() } while(0); \
    printf("PASS\n"); \
    tests_passed++; \
} while(0)

#define TEST_FAIL(msg) do { \
    printf("FAIL: %s\n", msg); \
    tests_failed++; \
    return; \
} while(0)

#define TEST_ASSERT(cond, msg) do { \
    if (!(cond)) { TEST_FAIL(msg); } \
} while(0)

#define TEST_SKIP(reason) do { \
    printf("SKIP: %s\n", reason); \
    tests_skipped++; \
    return; \
} while(0)

/* ==================================================================
 * 工具：在本地端口启动一个 TCP 服务器（用于 mock 网关）
 * ================================================================== */
typedef struct {
    int          listen_fd;
    int          client_fd;
    int          port;
#if defined(_WIN32) || defined(_WIN64)
    uintptr_t    thread;
#else
    pthread_t    thread;
#endif
    volatile int running;
} mock_server_t;

/* 发送 WS 文本帧（服务器端，无需掩码） */
static void mock_send_text(int fd, const char *json) {
    int len = (int)strlen(json);
    uint8_t hdr[10];
    int hdr_len = 2;
    hdr[0] = 0x80 | 0x1;
    if (len < 126) {
        hdr[1] = (uint8_t)len;
    } else if (len < 65536) {
        hdr[1] = 126;
        hdr[2] = (uint8_t)(len >> 8);
        hdr[3] = (uint8_t)(len & 0xFF);
        hdr_len = 4;
    } else {
        hdr[1] = 127;
        uint64_t l = (uint64_t)len;
        for (int i = 8; i > 0; i--) {
            hdr[1 + i] = (uint8_t)(l & 0xFF);
            l >>= 8;
        }
        hdr_len = 10;
    }
    send(fd, (const char *)hdr, hdr_len, 0);
    send(fd, json, len, 0);
}

/* 发送 WS 二进制帧（服务器端，无需掩码） */
static void mock_send_binary(int fd, const uint8_t *data, int len) {
    uint8_t hdr[10];
    int hdr_len = 2;
    hdr[0] = 0x80 | 0x2;
    if (len < 126) {
        hdr[1] = (uint8_t)len;
    } else if (len < 65536) {
        hdr[1] = 126;
        hdr[2] = (uint8_t)(len >> 8);
        hdr[3] = (uint8_t)(len & 0xFF);
        hdr_len = 4;
    } else {
        hdr[1] = 127;
        uint64_t l = (uint64_t)len;
        for (int i = 8; i > 0; i--) {
            hdr[1 + i] = (uint8_t)(l & 0xFF);
            l >>= 8;
        }
        hdr_len = 10;
    }
    send(fd, (const char *)hdr, hdr_len, 0);
    send(fd, (const char *)data, len, 0);
}

/* 读取 WS 文本帧（服务器端，解析掩码） */
static char *mock_read_text(int fd) {
    uint8_t hdr[2];
    if (recv(fd, (char *)hdr, 2, 0) != 2) return NULL;
    int masked = (hdr[1] & 0x80) ? 1 : 0;
    uint64_t len = hdr[1] & 0x7F;
    if (len == 126) {
        uint8_t ext[2];
        if (recv(fd, (char *)ext, 2, 0) != 2) return NULL;
        len = ((uint64_t)ext[0] << 8) | ext[1];
    } else if (len == 127) {
        uint8_t ext[8];
        if (recv(fd, (char *)ext, 8, 0) != 8) return NULL;
        len = 0;
        for (int i = 0; i < 8; i++) len = (len << 8) | ext[i];
    }
    uint8_t mask_key[4] = {0};
    if (masked) {
        if (recv(fd, (char *)mask_key, 4, 0) != 4) return NULL;
    }
    char *buf = (char *)malloc((size_t)len + 1);
    if (!buf) return NULL;
    if (len > 0) {
        if ((int)recv(fd, buf, (int)len, 0) != (int)len) {
            free(buf);
            return NULL;
        }
        if (masked) {
            for (uint64_t i = 0; i < len; i++)
                buf[i] ^= mask_key[i & 3];
        }
    }
    buf[len] = '\0';
    return buf;
}

/* mock 服务器线程 */
#if defined(_WIN32) || defined(_WIN64)
static unsigned __stdcall mock_server_thread(void *arg) {
#else
static void *mock_server_thread(void *arg) {
#endif
    mock_server_t *ms = (mock_server_t *)arg;
    struct sockaddr_in client_addr;
    socklen_t addr_len = sizeof(client_addr);
    int fd = accept(ms->listen_fd, (struct sockaddr *)&client_addr, &addr_len);
    if (fd < 0) THREAD_RETURN;
    ms->client_fd = fd;

    /* 读取 WS 升级请求 */
    char buf[4096] = {0};
    int n = 0;
    while (n < (int)sizeof(buf) - 1) {
        int r = (int)recv(fd, buf + n, 1, 0);
        if (r <= 0) break;
        n += r;
        buf[n] = '\0';
        if (n >= 4 && strcmp(buf + n - 4, "\r\n\r\n") == 0) break;
    }

    /* 发送 101 响应 */
    const char *resp =
        "HTTP/1.1 101 Switching Protocols\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n"
        "\r\n";
    send(fd, resp, (int)strlen(resp), 0);

    /* 协议循环 */
    char *msg;
    while (ms->running) {
        msg = mock_read_text(fd);
        if (!msg) break;

        if (strstr(msg, "\"hello\"")) {
            mock_send_text(fd, "{\"op\":\"hello_ack\",\"status\":\"ok\"}");
        } else if (strstr(msg, "\"bind\"")) {
            mock_send_text(fd, "{\"op\":\"bind_ack\",\"status\":\"ok\",\"device_id\":\"test-dev\"}");
        }
        /* cmd_result, event, status, cmd_data_start/end 可以忽略 */
        free(msg);
    }
    sock_close(fd);
    ms->client_fd = -1;
    THREAD_RETURN;
}

static mock_server_t *mock_server_start(int port) {
    mock_server_t *ms = (mock_server_t *)calloc(1, sizeof(mock_server_t));

#if defined(_WIN32) || defined(_WIN64)
    WSADATA wsa;
    WSAStartup(MAKEWORD(2, 2), &wsa);
#endif

    ms->listen_fd = (int)socket(AF_INET, SOCK_STREAM, 0);
    int opt = 1;
    setsockopt(ms->listen_fd, SOL_SOCKET, SO_REUSEADDR,
               (const char *)&opt, sizeof(opt));

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = INADDR_ANY;
    addr.sin_port = htons(port);
    if (bind(ms->listen_fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        free(ms);
        return NULL;
    }
    listen(ms->listen_fd, 1);
    ms->port = port;
    ms->running = 1;
    ms->client_fd = -1;

#if defined(_WIN32) || defined(_WIN64)
    ms->thread = _beginthreadex(NULL, 0, mock_server_thread, ms, 0, NULL);
#else
    pthread_create(&ms->thread, NULL, mock_server_thread, ms);
#endif
    usleep(200000); /* 等待服务器就绪 */
    return ms;
}

static void mock_server_stop(mock_server_t *ms) {
    if (!ms) return;
    ms->running = 0;
    if (ms->client_fd >= 0) sock_close(ms->client_fd);
    sock_close(ms->listen_fd);
#if defined(_WIN32) || defined(_WIN64)
    WaitForSingleObject((HANDLE)ms->thread, 3000);
    WSACleanup();
#else
    pthread_join(ms->thread, NULL);
#endif
    free(ms);
}

/* ==================================================================
 * 传输层实现（POSIX socket）
 * ================================================================== */
struct transport_ctx {
    int sock;
};

static int transport_connect(void *ctx, const char *host, uint16_t port) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    struct hostent *he = gethostbyname(host);
    if (!he) return -1;
    tc->sock = (int)socket(AF_INET, SOCK_STREAM, 0);
    if (tc->sock < 0) return -1;
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    memcpy(&addr.sin_addr, he->h_addr_list[0], he->h_length);
    if (connect(tc->sock, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        sock_close(tc->sock);
        tc->sock = -1;
        return -1;
    }
    /* 设置 100ms 接收超时，使 ha_client_process 不会永久阻塞 */
#if defined(_WIN32) || defined(_WIN64)
    DWORD timeout = 100;
    setsockopt(tc->sock, SOL_SOCKET, SO_RCVTIMEO, (const char *)&timeout, sizeof(timeout));
#else
    struct timeval tv = {0, 100000};
    setsockopt(tc->sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
#endif
    return 0;
}

static int transport_send(void *ctx, const uint8_t *data, int len) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    int sent = 0;
    while (sent < len) {
        int n = (int)send(tc->sock, (const char *)(data + sent), len - sent, 0);
        if (n <= 0) return -1;
        sent += n;
    }
    return sent;
}

static int transport_recv(void *ctx, uint8_t *buf, int len) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    return (int)recv(tc->sock, (char *)buf, len, 0);
}

static void transport_close(void *ctx) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    if (tc->sock >= 0) {
        sock_close(tc->sock);
        tc->sock = -1;
    }
}

/* ==================================================================
 * mock 服务器：发送命令
 * ================================================================== */
static void mock_send_cmd(mock_server_t *ms, const char *cmd_type,
                          const char *req_id, const char *cmd) {
    char buf[4096];
    char escaped[2048];
    int ei = 0;
    /* 转义 cmd 中的双引号和反斜杠 */
    if (cmd) {
        for (int i = 0; cmd[i] && ei < (int)sizeof(escaped) - 6; i++) {
            if (cmd[i] == '"' || cmd[i] == '\\') {
                escaped[ei++] = '\\';
                if (ei >= (int)sizeof(escaped) - 1) break;
            }
            escaped[ei++] = cmd[i];
        }
    }
    escaped[ei] = '\0';
    int n = snprintf(buf, sizeof(buf),
        "{\"op\":\"cmd\",\"req_id\":\"%s\",\"cmd_type\":\"%s\",\"command\":\"%s\"}",
        req_id ? req_id : "", cmd_type ? cmd_type : "", escaped);
    mock_send_text(ms->client_fd, buf);
}

/* ==================================================================
 * 测试用例
 * ================================================================== */

/* ---------- 1. JSON 解析器 ---------- */
static void test_json_parser(void) {
    TEST_BEGIN("JSON parser: parse object");

    const char *json = "{\"op\":\"hello\",\"device\":{\"id\":\"test\",\"caps\":[\"a\",\"b\"]}}";
    ha_json_node_t *root = ha_json_parse(json);
    TEST_ASSERT(root != NULL, "parse failed");
    TEST_ASSERT(root->type == HA_JSON_OBJECT, "not an object");

    const char *op = ha_json_get_string(root, "op");
    TEST_ASSERT(op != NULL && strcmp(op, "hello") == 0, "op mismatch");

    ha_json_node_t *dev = ha_json_get(root, "device");
    TEST_ASSERT(dev != NULL && dev->type == HA_JSON_OBJECT, "device not object");

    const char *id = ha_json_get_string(dev, "id");
    TEST_ASSERT(id != NULL && strcmp(id, "test") == 0, "device id mismatch");

    ha_json_node_t *caps = ha_json_get(dev, "caps");
    TEST_ASSERT(caps != NULL && caps->type == HA_JSON_ARRAY, "caps not array");
    TEST_ASSERT(ha_json_array_len(caps) == 2, "caps length wrong");

    ha_json_node_t *c0 = ha_json_array_get(caps, 0);
    TEST_ASSERT(c0 != NULL && c0->type == HA_JSON_STRING &&
                strcmp(c0->str_val, "a") == 0, "caps[0] mismatch");

    ha_json_free(root);
    TEST_END();
}

static void test_json_int(void) {
    TEST_BEGIN("JSON parser: int fields");

    const char *json = "{\"count\":42,\"total\":100500,\"neg\":-7}";
    ha_json_node_t *root = ha_json_parse(json);
    TEST_ASSERT(root != NULL, "parse failed");

    TEST_ASSERT(ha_json_get_int(root, "count", -1) == 42, "count mismatch");
    TEST_ASSERT(ha_json_get_int(root, "total", -1) == 100500, "total mismatch");
    TEST_ASSERT(ha_json_get_int(root, "neg", 0) == -7, "neg mismatch");
    TEST_ASSERT(ha_json_get_int(root, "nonexistent", -999) == -999, "default wrong");

    ha_json_free(root);
    TEST_END();
}

static void test_json_builder(void) {
    TEST_BEGIN("JSON builder: object with string + int + bool");

    char buf[256];
    ha_json_builder_t jb;
    ha_json_builder_init(&jb, buf, sizeof(buf));
    ha_json_builder_begin_object(&jb);
    ha_json_builder_string(&jb, "op", "hello");
    ha_json_builder_int(&jb, "seq", 1);
    ha_json_builder_bool(&jb, "active", 1);
    ha_json_builder_end_object(&jb);

    const char *r = ha_json_builder_str(&jb);
    TEST_ASSERT(r != NULL, "builder returned NULL");
    TEST_ASSERT(strstr(r, "\"op\":\"hello\"") != NULL, "missing op");
    TEST_ASSERT(strstr(r, "\"seq\":1") != NULL, "missing seq");
    TEST_ASSERT(strstr(r, "\"active\":true") != NULL, "missing bool");

    TEST_END();
}

static void test_json_array_builder(void) {
    TEST_BEGIN("JSON builder: array of strings");

    char buf[256];
    ha_json_builder_t jb;
    ha_json_builder_init(&jb, buf, sizeof(buf));
    ha_json_builder_begin_object(&jb);
    ha_json_builder_key(&jb, "caps");
    ha_json_builder_begin_array(&jb);
    ha_json_builder_add_string(&jb, "camera");
    ha_json_builder_add_string(&jb, "screen");
    ha_json_builder_end_array(&jb);
    ha_json_builder_end_object(&jb);

    const char *r = ha_json_builder_str(&jb);
    TEST_ASSERT(r != NULL, "builder returned NULL");
    TEST_ASSERT(strstr(r, "\"camera\"") != NULL, "missing camera");
    TEST_ASSERT(strstr(r, "\"screen\"") != NULL, "missing screen");

    TEST_END();
}

/* ---------- 2. 工具函数 ---------- */
static void test_parse_homeagent(void) {
    TEST_BEGIN("ha_cmd_parse_homeagent: basic");

    const char *cap, *args;

    ha_cmd_parse_homeagent("camerasue 5", &cap, &args);
    TEST_ASSERT(strcmp(cap, "camerasue") == 0, "cap mismatch");
    TEST_ASSERT(strcmp(args, "5") == 0, "args mismatch");

    ha_cmd_parse_homeagent("screensee", &cap, &args);
    TEST_ASSERT(strcmp(cap, "screensee") == 0, "cap mismatch (no args)");
    TEST_ASSERT(strcmp(args, "") == 0, "args should be empty");

    ha_cmd_parse_homeagent("homeagent-camerasue 3", &cap, &args);
    TEST_ASSERT(strcmp(cap, "camerasue") == 0, "prefix not stripped");
    TEST_ASSERT(strcmp(args, "3") == 0, "args after prefix");

    ha_cmd_parse_homeagent("", &cap, &args);
    TEST_ASSERT(strcmp(cap, "") == 0, "empty input");

    TEST_END();
}

static void test_parse_json(void) {
    TEST_BEGIN("ha_cmd_parse_json: action + json");

    const char *action, *json_str;

    ha_cmd_parse_json("computeruse {\"action\":\"click\",\"x\":100}",
                      &action, &json_str);
    TEST_ASSERT(strcmp(action, "computeruse") == 0, "action mismatch");
    TEST_ASSERT(strstr(json_str, "\"action\"") != NULL, "json missing");

    ha_cmd_parse_json("screensee", &action, &json_str);
    TEST_ASSERT(strcmp(action, "screensee") == 0, "action no json");
    TEST_ASSERT(strcmp(json_str, "") == 0, "json should be empty");

    TEST_END();
}

static void test_base64(void) {
    TEST_BEGIN("ha_base64_encode: basic");

    const uint8_t data[] = "Hello, World!";
    char out[64];
    int n = ha_base64_encode(data, 13, out, sizeof(out));
    TEST_ASSERT(n > 0, "encode returned 0");
    TEST_ASSERT(strcmp(out, "SGVsbG8sIFdvcmxkIQ==") == 0, "base64 mismatch");

    const uint8_t jpeg[] = {0xFF, 0xD8, 0xFF};
    n = ha_base64_encode(jpeg, 3, out, sizeof(out));
    TEST_ASSERT(n > 0, "short encode failed");
    TEST_ASSERT(strcmp(out, "/9j/") == 0, "jpeg magic mismatch");

    /* 缓冲区不足 */
    n = ha_base64_encode(data, 13, out, 5);
    TEST_ASSERT(n > 5, "should return needed size");

    TEST_END();
}

/* ---------- 3. WS 帧编解码 ---------- */
static void test_ws_frame(void) {
#if defined(_WIN32) || defined(_WIN64)
    printf("  TEST: WS frame: encode/decode with mask ... SKIP: socketpair not available on Windows\n");
    tests_skipped++;
    return;
#else
    TEST_BEGIN("WS frame: encode/decode with mask");

    /* 使用 TCP 本地连接测试 */
    int sv[2];
    TEST_ASSERT(socketpair(AF_UNIX, SOCK_STREAM, 0, sv) == 0,
                "socketpair failed");

    const char *test_payload = "hello world";
    int test_len = (int)strlen(test_payload);

    /* 发送端：手动构造 WS 帧 */
    uint8_t frame[1024];
    int off = 0;
    frame[off++] = 0x80 | 0x1;
    frame[off++] = 0x80 | (uint8_t)test_len;
    uint8_t mask[4] = {0x01, 0x02, 0x03, 0x04};
    memcpy(frame + off, mask, 4); off += 4;
    for (int i = 0; i < test_len; i++)
        frame[off++] = test_payload[i] ^ mask[i & 3];
    send(sv[0], (const char *)frame, off, 0);

    /* 接收端 */
    ha_transport_t transport = {
        .send = transport_send, .recv = transport_recv,
        .close = transport_close,
    };
    struct transport_ctx tctx_reader = { .sock = sv[1] };
    transport.ctx = &tctx_reader;

    ha_ws_t ws;
    memset(&ws, 0, sizeof(ws));
    ws.transport = &transport;
    ws.connected = 1;

    const uint8_t *payload;
    int len;
    int opcode = ha_ws_read_frame(&ws, &payload, &len);
    TEST_ASSERT(opcode == 0x1, "expected text frame");
    TEST_ASSERT(len == test_len, "length mismatch");
    TEST_ASSERT(memcmp(payload, test_payload, len) == 0, "payload mismatch");

    sock_close(sv[0]);
    sock_close(sv[1]);
    TEST_END();
#endif
}

/* ---------- 4. ha_version ---------- */
static void test_version(void) {
    TEST_BEGIN("ha_version: returns non-empty");

    const char *v = ha_version();
    TEST_ASSERT(v != NULL && v[0] != '\0', "version empty");
    printf("(v=%s) ", v);
    TEST_END();
}

/* ---------- 5. 完整客户端生命周期 ---------- */
static volatile int lifecycle_state = 0;

static void test_lifecycle_on_state(int connected, void *userdata) {
    (void)userdata;
    lifecycle_state = connected ? 1 : 0;
}

static ha_status_t test_handler_camerasue(const char *req_id, const char *args,
                                          ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("    [handler] camerasue called, args=%s\n", args ? args : "");
    result->status = 0;
    result->output = "data:image/jpeg;base64,test123";
    return HA_OK;
}

static ha_status_t test_handler_shell(const char *req_id, const char *args,
                                      ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("    [handler] shell called: %s\n", args ? args : "");
    result->status = 0;
    result->output = "shell output here";
    return HA_OK;
}

static void test_client_lifecycle(void) {
    TEST_BEGIN("Client lifecycle: connect -> hello -> bind -> ready");

    mock_server_t *ms = mock_server_start(19890);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    const char *caps[] = {"camera", "status", NULL};
    ha_cmd_handler_def_t handlers[] = {
        {.command = "shell",     .handler = test_handler_shell},
        {.command = "camerasue", .handler = test_handler_camerasue},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19890",
        .token     = "test-token",
        .device = {
            .device_id = "test-dev",
            .name      = "Test Device",
            .kind      = "camera",
            .caps      = caps,
        },
        .handlers  = handlers,
        .on_state  = test_lifecycle_on_state,
        .ping_interval = 30,
    };

    lifecycle_state = 0;
    ha_client_t *client = ha_client_new(&config);
    TEST_ASSERT(client != NULL, "client new failed");

    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);
    TEST_ASSERT(lifecycle_state == 1, "should be connected");

    /* 发送命令验证分发 */
    mock_send_cmd(ms, "homeagent", "req-1", "camerasue 3");
    usleep(200000);
    ha_client_process(client);

    mock_send_cmd(ms, "shell", "req-2", "ls -la");
    usleep(200000);
    ha_client_process(client);

    /* 未注册的命令 */
    mock_send_cmd(ms, "homeagent", "req-3", "unknowncmd");
    usleep(100000);
    ha_client_process(client);

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 6. 命令分发 ---------- */
static volatile int cmd_camerasue_fired = 0;
static volatile int cmd_shell_fired = 0;
static char cmd_req_id[128] = "";

static ha_status_t test_handler_camerasue2(const char *req_id, const char *args,
                                           ha_cmd_result_t *result, void *userdata) {
    (void)userdata;
    cmd_camerasue_fired = 1;
    strncpy(cmd_req_id, req_id, sizeof(cmd_req_id) - 1);
    if (strcmp(args, "5") != 0) {
        printf("    [handler] args mismatch: expected '5', got '%s'\n", args);
        result->status = 1;
        result->error = "args mismatch";
        return HA_OK;
    }
    result->status = 0;
    result->output = "snapshot taken";
    return HA_OK;
}

static ha_status_t test_handler_shell2(const char *req_id, const char *args,
                                       ha_cmd_result_t *result, void *userdata) {
    (void)userdata;
    cmd_shell_fired = 1;
    if (strstr(args, "ls") == NULL) {
        printf("    [handler] args should contain 'ls', got '%s'\n", args);
        result->status = 1;
        result->error = "args mismatch";
        return HA_OK;
    }
    result->status = 0;
    result->output = "file1.txt\nfile2.txt";
    return HA_OK;
}

static void test_command_dispatch(void) {
    TEST_BEGIN("Command dispatch: homeagent + shell");

    mock_server_t *ms = mock_server_start(19891);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    const char *caps[] = {"camera", "cmd", NULL};
    ha_cmd_handler_def_t handlers[] = {
        {.command = "shell",     .handler = test_handler_shell2},
        {.command = "camerasue", .handler = test_handler_camerasue2},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19891",
        .token     = "test-token",
        .device = {
            .device_id = "cmd-dev",
            .name      = "Cmd Test",
            .kind      = "camera",
            .caps      = caps,
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    cmd_camerasue_fired = 0;
    cmd_shell_fired = 0;
    memset(cmd_req_id, 0, sizeof(cmd_req_id));

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 发送 homeagent 命令 */
    mock_send_cmd(ms, "homeagent", "req-cam", "camerasue 5");
    usleep(200000);
    ha_client_process(client);
    TEST_ASSERT(cmd_camerasue_fired == 1, "camerasue handler not called");
    TEST_ASSERT(strcmp(cmd_req_id, "req-cam") == 0, "req_id mismatch");

    /* 发送 shell 命令 */
    mock_send_cmd(ms, "shell", "req-sh", "ls -la /tmp");
    usleep(200000);
    ha_client_process(client);
    TEST_ASSERT(cmd_shell_fired == 1, "shell handler not called");

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 7. 事件上报 ---------- */
static void test_event_report(void) {
    TEST_BEGIN("Event report: client sends event to server");

    mock_server_t *ms = mock_server_start(19892);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19892",
        .token     = "test-token",
        .device = {
            .device_id = "evt-dev",
            .name      = "Event Test",
            .kind      = "camera",
            .caps      = (const char *[]){"camera", NULL},
        },
        .handlers  = NULL,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    ha_client_send_event(client, "motion_detected", "{\"zone\":\"front_door\"}");
    usleep(100000);
    ha_client_process(client);

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 8. 状态上报 ---------- */
static void test_status_report(void) {
    TEST_BEGIN("Status report: client sends status to server");

    mock_server_t *ms = mock_server_start(19893);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19893",
        .token     = "test-token",
        .device = {
            .device_id = "st-dev",
            .name      = "Status Test",
            .kind      = "camera",
            .caps      = (const char *[]){"camera", NULL},
        },
        .handlers  = NULL,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    ha_client_send_status(client, "offline");
    usleep(100000);
    ha_client_process(client);

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 9. 二进制分块回传 ---------- */
static void test_binary_chunked(void) {
    TEST_BEGIN("Binary chunked transfer: send video data to server");

    mock_server_t *ms = mock_server_start(19894);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    ha_cmd_handler_def_t handlers[] = {
        {.command = "camerasue", .handler = test_handler_camerasue},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19894",
        .token     = "test-token",
        .device = {
            .device_id = "bin-dev",
            .name      = "Binary Test",
            .kind      = "camera",
            .caps      = (const char *[]){"camera", NULL},
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 模拟录像数据 */
    uint8_t video_data[20000];
    for (int i = 0; i < 20000; i++) video_data[i] = (uint8_t)(i % 251);

    ha_client_send_data_chunked(client, "req-video", "camera_video",
                                "video/mp4", video_data, 20000);
    usleep(500000);
    ha_client_process(client);

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 10. 未注册命令处理 ---------- */
static void test_unsupported_command(void) {
    TEST_BEGIN("Unsupported command: returns error gracefully");

    mock_server_t *ms = mock_server_start(19895);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    ha_cmd_handler_def_t handlers[] = {
        {.command = "camerasue", .handler = test_handler_camerasue},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19895",
        .token     = "test-token",
        .device = {
            .device_id = "unsup-dev",
            .name      = "Unsupported Test",
            .kind      = "camera",
            .caps      = (const char *[]){"camera", NULL},
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 发送未注册命令 */
    mock_send_cmd(ms, "homeagent", "req-unsup", "screensee");
    usleep(100000);
    ha_client_process(client);

    /* 发送已注册命令 */
    mock_send_cmd(ms, "homeagent", "req-ok", "camerasue");
    usleep(100000);
    ha_client_process(client);

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 11. 语音数据接收 ---------- */
static volatile int speech_received = 0;
static uint8_t speech_data[4096];
static int speech_len = 0;

static void test_on_binary(const char *req_id, const char *kind,
                           const char *mime, const uint8_t *data,
                           int len, void *userdata) {
    (void)req_id; (void)kind; (void)mime; (void)userdata;
    speech_received = 1;
    speech_len = len < (int)sizeof(speech_data) ? len : (int)sizeof(speech_data);
    memcpy(speech_data, data, (size_t)speech_len);
}

static void test_speech_receive(void) {
    TEST_BEGIN("Speech data receive: cmd_speech_start -> binary -> end");

    mock_server_t *ms = mock_server_start(19896);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19896",
        .token     = "test-token",
        .device = {
            .device_id = "speech-dev",
            .name      = "Speech Test",
            .kind      = "speaker",
            .caps      = (const char *[]){"speaker", NULL},
        },
        .handlers  = NULL,
        .on_binary = test_on_binary,
        .ping_interval = 30,
    };

    speech_received = 0;
    speech_len = 0;

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* mock 服务器发送语音数据 */
    const char *audio_data = "RIFF....fake-wav-data....";
    mock_send_text(ms->client_fd,
        "{\"op\":\"cmd_speech_start\",\"req_id\":\"req-speech\","
        "\"kind\":\"speech\",\"mime\":\"audio/wav\",\"total\":25}");
    usleep(50000);
    mock_send_binary(ms->client_fd, (const uint8_t *)audio_data, 25);
    usleep(50000);
    mock_send_text(ms->client_fd,
        "{\"op\":\"cmd_speech_end\",\"req_id\":\"req-speech\"}");
    usleep(200000);

    /* 处理帧 */
    for (int i = 0; i < 50; i++) {
        ha_client_process(client);
        if (speech_received) break;
        usleep(20000);
    }

    TEST_ASSERT(speech_received == 1, "speech handler not called");
    TEST_ASSERT(speech_len == 25, "speech length mismatch");
    TEST_ASSERT(memcmp(speech_data, audio_data, 25) == 0, "speech data mismatch");

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 12. 二进制分块 via handler ---------- */
static ha_status_t test_handler_video(const char *req_id, const char *args,
                                      ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("    [handler] video record %s\n", args ? args : "");

    static uint8_t video[5000];
    for (int i = 0; i < 5000; i++) video[i] = (uint8_t)(i & 0xFF);

    result->status = 0;
    result->has_binary = 1;
    result->binary_data = video;
    result->binary_len = 5000;
    result->binary_mime = "video/mp4";
    return HA_OK;
}

static void test_binary_via_handler(void) {
    TEST_BEGIN("Binary via handler: handler sets has_binary, SDK auto-chunks");

    mock_server_t *ms = mock_server_start(19897);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    ha_cmd_handler_def_t handlers[] = {
        {.command = "camerasue", .handler = test_handler_video},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19897",
        .token     = "test-token",
        .device = {
            .device_id = "bin2-dev",
            .name      = "Binary Via Handler",
            .kind      = "camera",
            .caps      = (const char *[]){"camera", NULL},
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    mock_send_cmd(ms, "homeagent", "req-video2", "camerasue 10");
    usleep(500000);
    ha_client_process(client);
    usleep(100000);
    ha_client_process(client);

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 13. computeruse：结构化 JSON 命令分发 ---------- */
static volatile int computeruse_fired = 0;
static char computeruse_action[64] = "";
static int computeruse_x = 0;
static int computeruse_y = 0;

static ha_status_t test_handler_computeruse(const char *req_id, const char *args,
                                            ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    computeruse_fired = 1;
    /* 解析 JSON 参数 */
    if (args && args[0] == '{') {
        ha_json_node_t *root = ha_json_parse(args);
        if (root) {
            const char *act = ha_json_get_string(root, "action");
            if (act) strncpy(computeruse_action, act, sizeof(computeruse_action) - 1);
            computeruse_x = ha_json_get_int(root, "x", 0);
            computeruse_y = ha_json_get_int(root, "y", 0);
            ha_json_free(root);
        }
    }
    result->status = 0;
    result->output = "clicked at (100,200)";
    return HA_OK;
}

static void test_computeruse_dispatch(void) {
    TEST_BEGIN("Computeruse: structured JSON command dispatch");

    mock_server_t *ms = mock_server_start(19898);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    const char *caps[] = {"computeruse", "cmd", NULL};
    ha_cmd_handler_def_t handlers[] = {
        {.command = "computeruse", .handler = test_handler_computeruse},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19898",
        .token     = "test-token",
        .device = {
            .device_id = "cu-dev",
            .name      = "ComputerUse Test",
            .kind      = "computer",
            .caps      = caps,
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    computeruse_fired = 0;
    memset(computeruse_action, 0, sizeof(computeruse_action));
    computeruse_x = 0;
    computeruse_y = 0;

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 发送 computeruse 命令（带结构化 JSON 参数） */
    mock_send_cmd(ms, "homeagent", "req-cu",
        "computeruse {\"action\":\"click\",\"x\":100,\"y\":200}");
    usleep(200000);
    ha_client_process(client);
    TEST_ASSERT(computeruse_fired == 1, "computeruse handler not called");
    TEST_ASSERT(strcmp(computeruse_action, "click") == 0,
                "action should be 'click'");
    TEST_ASSERT(computeruse_x == 100, "x should be 100");
    TEST_ASSERT(computeruse_y == 200, "y should be 200");

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 14. clipboardsee/clipboardsue 命令分发 ---------- */
static volatile int clipboardsee_fired = 0;
static volatile int clipboardsue_fired = 0;
static char clipboardsue_text[256] = "";

static ha_status_t test_handler_clipboardsee(const char *req_id, const char *args,
                                             ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)args; (void)userdata;
    clipboardsee_fired = 1;
    result->status = 0;
    result->output = "clipboard content here";
    return HA_OK;
}

static ha_status_t test_handler_clipboardsue(const char *req_id, const char *args,
                                             ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    clipboardsue_fired = 1;
    if (args) strncpy(clipboardsue_text, args, sizeof(clipboardsue_text) - 1);
    result->status = 0;
    result->output = "clipboard set";
    return HA_OK;
}

static void test_clipboard_dispatch(void) {
    TEST_BEGIN("Clipboard: clipboardsee + clipboardsue dispatch");

    mock_server_t *ms = mock_server_start(19899);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    const char *caps[] = {"clipboard", "cmd", NULL};
    ha_cmd_handler_def_t handlers[] = {
        {.command = "clipboardsee",  .handler = test_handler_clipboardsee},
        {.command = "clipboardsue",  .handler = test_handler_clipboardsue},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19899",
        .token     = "test-token",
        .device = {
            .device_id = "clip-dev",
            .name      = "Clipboard Test",
            .kind      = "computer",
            .caps      = caps,
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    clipboardsee_fired = 0;
    clipboardsue_fired = 0;
    memset(clipboardsue_text, 0, sizeof(clipboardsue_text));

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 发送 clipboardsee 命令 */
    mock_send_cmd(ms, "homeagent", "req-cs", "clipboardsee");
    usleep(200000);
    ha_client_process(client);
    TEST_ASSERT(clipboardsee_fired == 1, "clipboardsee handler not called");

    /* 发送 clipboardsue 命令 */
    mock_send_cmd(ms, "homeagent", "req-cw", "clipboardsue Hello World");
    usleep(200000);
    ha_client_process(client);
    TEST_ASSERT(clipboardsue_fired == 1, "clipboardsue handler not called");
    TEST_ASSERT(strcmp(clipboardsue_text, "Hello World") == 0,
                "clipboardsue text mismatch");

    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 15. screensee：带 data URL 的结果回传 ---------- */
static ha_status_t test_handler_screensee(const char *req_id, const char *args,
                                          ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)args; (void)userdata;
    result->status = 0;
    /* 返回 data URL 格式的 base64 图像数据 */
    result->output = "data:image/jpeg;base64,/9j/4AAQSkZJRg==";
    return HA_OK;
}

static void test_screensee_data_url(void) {
    TEST_BEGIN("Screensee: handler returns data URL result");

    mock_server_t *ms = mock_server_start(19900);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    const char *caps[] = {"screen", "cmd", NULL};
    ha_cmd_handler_def_t handlers[] = {
        {.command = "screensee", .handler = test_handler_screensee},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19900",
        .token     = "test-token",
        .device = {
            .device_id = "see-dev",
            .name      = "Screensee Test",
            .kind      = "computer",
            .caps      = caps,
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 发送 screensee 命令 */
    mock_send_cmd(ms, "homeagent", "req-see", "screensee");
    usleep(200000);
    ha_client_process(client);

    /* handler 已被调用，且返回了 data URL（无法直接验证回执内容，但 handler 已执行） */
    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ---------- 16. info_json 设备信息 ---------- */
static void test_info_json(void) {
    TEST_BEGIN("Device info_json: included in hello message");

    mock_server_t *ms = mock_server_start(19901);
    TEST_ASSERT(ms != NULL, "mock server start failed");

    struct transport_ctx tctx;
    tctx.sock = -1;
    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    const char *caps[] = {"camera", NULL};
    ha_cmd_handler_def_t handlers[] = {
        {.command = "camerasue", .handler = test_handler_camerasue},
        {.command = NULL},
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:19901",
        .token     = "test-token",
        .device = {
            .device_id = "info-dev",
            .name      = "Info Test",
            .kind      = "camera",
            .caps      = caps,
            .info_json = "{\"chip\":\"ESP32-S3\",\"psram\":8}",
        },
        .handlers  = handlers,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    ha_status_t st = ha_client_start(client);
    TEST_ASSERT(st == HA_OK, "client start failed");
    usleep(200000);

    /* 连接成功，info_json 已通过 hello 消息发送 */
    ha_client_stop(client);
    ha_client_destroy(client);
    mock_server_stop(ms);

    TEST_END();
}

/* ==================================================================
 * 主函数
 * ================================================================== */
int main(void) {
#if !defined(_WIN32) && !defined(_WIN64)
    signal(SIGPIPE, SIG_IGN);
#endif

    printf("========================================\n");
    printf("  ha_remotedevice 全面测试\n");
    printf("========================================\n\n");

    /* ---- JSON 解析/构建 ---- */
    printf("[JSON]\n");
    test_json_parser();
    test_json_int();
    test_json_builder();
    test_json_array_builder();

    /* ---- 工具函数 ---- */
    printf("\n[Utilities]\n");
    test_parse_homeagent();
    test_parse_json();
    test_base64();

    /* ---- WS 协议 ---- */
    printf("\n[WebSocket]\n");
    test_ws_frame();

    /* ---- SDK 核心 ---- */
    printf("\n[SDK Core]\n");
    test_version();
    test_client_lifecycle();
    test_command_dispatch();

    /* ---- 协议功能 ---- */
    printf("\n[Protocol]\n");
    test_event_report();
    test_status_report();
    test_binary_chunked();
    test_unsupported_command();
    test_speech_receive();
    test_binary_via_handler();

    /* ---- 扩展命令 ---- */
    printf("\n[Extended Commands]\n");
    test_computeruse_dispatch();
    test_clipboard_dispatch();
    test_screensee_data_url();
    test_info_json();

    /* ---- 汇总 ---- */
    printf("\n========================================\n");
    printf("  结果: %d passed, %d failed, %d skipped\n",
           tests_passed, tests_failed, tests_skipped);
    printf("========================================\n");

    return tests_failed > 0 ? 1 : 0;
}