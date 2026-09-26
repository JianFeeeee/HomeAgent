/*
 * bench_ha_codec.c —— C 侧纯函数微基准（无 cgo 边界成本）
 *
 * ============================ 为什么 Go 侧基准不够 ============================
 * Go 侧 codec_bench_test.go 测到的数 = **函数体成本 + cgo 边界成本**（约 30ns）
 * 两项混在一起。后果：看到某个场景慢，分不清该优化 C 函数体，还是该减少
 * 跨语言调用次数（或把循环整体 C 化批量传一次）—— 而这三者的处方完全不同。
 * 只有在能隔离边界成本的地方（纯 C 循环）测，才知道该动谁。
 *
 * 用法：cmake -DBUILD_BENCH=ON && ./ha_codec_bench [reps]
 *
 * 覆盖与 Go 侧 benchInputs 对齐（empty / ascii_short / zh_short / zh_200 /
 * ascii_1k / zh_1k），便于两张表直接对读。
 */

#ifndef _POSIX_C_SOURCE
#  define _POSIX_C_SOURCE 199309L
#endif

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "ha_codec.h"

/* 单调时钟（纳秒）。
 *
 * ★ 必须是 clock_gettime（不是 clock()、不是 time()）：基准要测的是
 *   几十纳秒级的函数体耗时，clock()/time() 的分辨率是**秒**，
 *   拿它测 ns/op 只会得到一堆 0 或被量化成整数秒的噪声。
 *
 * ★ CLOCK_MONOTONIC 与 clock_gettime 都是 POSIX 而**非 ISO C99**，
 *   而 CMake 刻意设了 CMAKE_C_EXTENSIONS OFF（严格 -std=c99）
 *   ⇒ 未定义这两个符号。实测报错：
 *     error: storage size of 'ts' isn't known
 *     error: implicit declaration of function 'clock_gettime'
 *   这是 C 化门禁当场抓出的真实可移植性缺陷 —— 若靠 Makefile 的裸 gcc
 *   （默认 gnu17）构建，它会**静默编过**；而到别人的严格 C99 工具链上就炸。
 *
 *   故显式请求 POSIX 声明。_POSIX_C_SOURCE 必须在包含任何头文件**之前**
 *   定义（否则 feature test macro 无效，这也是最常见的踩法）。
 *   Windows/MSVC 走 _MSC_VER 分支（用 QueryPerformanceCounter），
 *   保证这个 bench 文件在异端也能编。 */
#if defined(_MSC_VER)
#  include <windows.h>
static double now_sec(void) {
    LARGE_INTEGER f, c;
    QueryPerformanceFrequency(&f);
    QueryPerformanceCounter(&c);
    return (double)c.QuadPart / (double)f.QuadPart;
}
#else
#  ifndef _POSIX_C_SOURCE
#    define _POSIX_C_SOURCE 199309L
#  endif
#  include <time.h>
static double now_sec(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}
#endif

/* 分配并填充 reps 个 'x' 的缓冲（可含 NUL 之外的任意字节）。 */
static char *make_fill(size_t n, char ch) {
    char *p = (char *)malloc(n ? n : 1);
    if (p) memset(p, ch, n);
    return p;
}

static void bench_estimate(const char *name, const char *s, size_t len, int reps) {
    /* 预热：把指令缓存与分支预测器带进稳态，否则首个样本的冷启动会
     * 均摊到很少的迭代上（reps 小的时候误差极大）。 */
    for (int i = 0; i < reps; i++) (void)ha_codec_estimate_tokens(s, len);

    double t0 = now_sec();
    int acc = 0;
    for (int i = 0; i < reps; i++) {
        acc += ha_codec_estimate_tokens(s, len);
    }
    double dt = now_sec() - t0;

    double ns = (reps > 0) ? (dt * 1e9 / reps) : 0.0;
    double mbs = (dt > 0) ? ((double)len * reps / dt / 1e6) : 0.0;
    printf("  %-12s len=%7zu  %9.2f ns/op  %8.1f MB/s  (acc=%d)\n",
           name, len, ns, mbs, acc);
}

static void bench_truncate(const char *name, const char *s, size_t len,
                           int max_tokens, int reps) {
    for (int i = 0; i < reps; i++) {
        (void)ha_codec_truncate_by_tokens(s, len, max_tokens);
    }
    double t0 = now_sec();
    size_t acc = 0;
    for (int i = 0; i < reps; i++) {
        acc += ha_codec_truncate_by_tokens(s, len, max_tokens);
    }
    double dt = now_sec() - t0;
    double ns = (reps > 0) ? (dt * 1e9 / reps) : 0.0;
    printf("  %-12s len=%7zu  %9.2f ns/op  (keep=%zu)\n",
           name, len, ns, acc / (size_t)reps);
}

int main(int argc, char **argv) {
    int reps = (argc > 1) ? atoi(argv[1]) : 200000;
    if (reps <= 0) reps = 200000;

    printf("== ha_codec C 侧微基准（reps=%d，纯 C 无 cgo 边界）==\n", reps);
    printf("-- ha_codec_estimate_tokens --\n");

    bench_estimate("empty", "", 0, reps);
    bench_estimate("ascii_short", "hello world", 11, reps);
    bench_estimate("zh_short", "用户询问了系统状态", 27, reps);
    {
        char *zh200 = make_fill(180, 'a'); /* 逐字节非 ASCII 由下方覆盖 */
        bench_estimate("ascii_200", zh200, 180, reps);
        free(zh200);
    }
    {
        char *zh = make_fill(1024, 'x');
        bench_estimate("ascii_1k", zh, 1024, reps);
        free(zh);
    }
    {
        /* 真实中文：每字 3 字节 = 1024 字节 ≈ 341 rune */
        char *zh = make_fill(1023, 'x');
        for (size_t i = 0; i + 2 < 1024; i += 3) {
            zh[i] = (char)0xE4; zh[i + 1] = (char)0xBD; zh[i + 2] = (char)0xA0;
        }
        bench_estimate("zh_1k", zh, 1024, reps);
        free(zh);
    }

    printf("-- ha_codec_truncate_by_tokens (max_tokens=64) --\n");
    {
        char *a1k = make_fill(1024, 'x');
        bench_truncate("ascii_1k", a1k, 1024, 64, reps);
        free(a1k);
    }
    {
        char *zh = make_fill(1023, 'x');
        for (size_t i = 0; i + 2 < 1024; i += 3) {
            zh[i] = (char)0xE4; zh[i + 1] = (char)0xBD; zh[i + 2] = (char)0xA0;
        }
        bench_truncate("zh_1k", zh, 1024, 64, reps);
        free(zh);
    }

    printf("-- ha_codec_model_context_window (含/不含匹配) --\n");
    {
        const char *models[4] = {
            "deepseek/deepseek-v4.1-flash", "gpt-4-turbo", "qwen-max", "AUTO"
        };
        for (int w = 0; w < 4; w++) {
            size_t l = strlen(models[w]);
            for (int i = 0; i < reps; i++) {
                (void)ha_codec_model_context_window(models[w], l);
            }
            double t0 = now_sec();
            int acc = 0;
            for (int i = 0; i < reps; i++) {
                acc += ha_codec_model_context_window(models[w], l);
            }
            double dt = now_sec() - t0;
            printf("  %-30s %9.2f ns/op (win=%d)\n", models[w],
                   (reps > 0) ? dt * 1e9 / reps : 0.0, acc / reps);
        }
    }

    printf("-- ABI --\n");
    printf("  ha_codec_abi_version = %d\n", ha_codec_abi_version());
    return 0;
}
