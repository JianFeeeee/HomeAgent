#ifndef HA_ABI_H
#define HA_ABI_H

/*
 * ha_abi.h — HomeAgent C 库的 ABI 版本契约
 *
 * ============================ 为什么需要它 ============================
 * ha_codec.h 声明「签名一经发布即冻结」，但**冻结只写在注释里**——注释不
 * 参与编译，Go/C 两侧对「我以为的版本」不一致时没有任何机制会报错。
 * 本头文件把冻结变成**编译期与测试期可断言的事实**：
 *
 *   1. 每个 C 库声明自己的 ABI 主/次版本（HA_CODEC_ABI_MAJOR/MINOR）。
 *   2. Go 侧（internal/agent/api/codec_cgo.go）持有一份 Go 常量副本，
 *      由 TestABIVersionMatches 比对 C 宏 —— 版本漂移**在测试里判红**，
 *      而不是等到线上表现为「插件行为诡异」才排查。
 *   3. 主版本不同 = ABI 不兼容，必须走大版本流程（与 homeagent-sdk 同一标准）。
 *
 * ============================ 改动规则 ============================
 *   - 只增不改、只加不改：新增函数/字段 → MINOR+1
 *   - 改签名、删函数、改结构体布局 → MAJOR+1（且所有调用方必须同步重编）
 *   - 纯内部实现优化（不动任何声明）→ 不动版本号
 *
 * ⚠️ 与 homeagent-sdk 的 C ABI 不同：本项目的 C 库是**源码内联编译**
 *   （Go 侧符号链接 csrc/ 权威源，见 codec_cgo.go 顶部），不存在跨版本
 *   混链的 .so/.a，所以「同批重建」是天然成立的——版本宏的作用是
 *   **防语义漂移**（两侧对同一组函数的理解不一致），不是防二进制不兼容。
 */

#ifdef __cplusplus
extern "C" {
#endif

/* ==================== 编译期断言（C99/C11 兼容） ==================== */

/* 静态断言：版本号写错必须在编译期就炸，不能带着荒谬版本号发布出去。
 *
 * ★ C99 没有 _Static_assert（那是 C11），而本项目 C 侧统一 -std=c99
 *   （见 codec_cgo.go 的 cgo CFLAGS 与 CMakeLists 的 C_STANDARD）。故需兼容垫片：
 *   C11+ 用原生 _Static_assert；C99 回退到「数组维度为 0 即编译失败」的老写法。
 *   这条垫片是 -Wall -Wextra -Wpedantic 门禁上线时**当场抓出来的**（首次编译即告警），
 *   即基础设施已经开始在发挥作用。
 *
 * 用法：第二个参数必须是**标识符**（不能是字符串）——C99 分支要用它 ## 成
 *   一个 typedef 名，而 `##` 不能拼接字符串字面量（拼接会直接编译报错）。
 *   原生 _Static_assert 分支则把它当 msg 传（此时它在诊断里显示为标识符，
 *   仍能指出是哪个断言）。同一文件内每个断言的 tag 必须不同。 */
#if defined(__STDC_VERSION__) && __STDC_VERSION__ >= 201112L
#  define HA_STATIC_ASSERT(cond, tag) _Static_assert(cond, #tag)
#elif defined(__cplusplus) && __cplusplus >= 201103L
#  define HA_STATIC_ASSERT(cond, tag) static_assert(cond, #tag)
#else
#  define HA_STATIC_ASSERT(cond, tag) \
      typedef char ha_sa_##tag##_line_##__LINE__[(cond) ? 1 : -1]
#endif

/* ==================== ABI 版本 ==================== */

/* 编码语义主版本：改动任一已发布函数的语义/签名时 +1。
 * 2026-09-26：首版 1.0（上下文窗口推断 + token 估算/截断）。 */
#define HA_CODEC_ABI_MAJOR 1

/* 编码语义次版本：纯新增（加函数、加枚举值）时 +1。 */
#define HA_CODEC_ABI_MINOR 0

/* 合成版号，便于日志/断言单值比较：major*1000 + minor */
#define HA_CODEC_ABI_VERSION (HA_CODEC_ABI_MAJOR * 1000 + HA_CODEC_ABI_MINOR)

/* 编译期锁死：ABI 版本必须落在「已知的、未被遗忘的」区间。
 * 若有人把版本号改成 0 或 999 之类（通常是手滑/拷贝粘贴出错），
 * 编译立即失败，而不是带着一个荒谬的版本号发布出去。 */
HA_STATIC_ASSERT(HA_CODEC_ABI_MAJOR >= 1 && HA_CODEC_ABI_MAJOR <= 9,
                 ha_codec_abi_major_in_range);
HA_STATIC_ASSERT(HA_CODEC_ABI_MINOR >= 0 && HA_CODEC_ABI_MINOR <= 99,
                 ha_codec_abi_minor_in_range);

#ifdef __cplusplus
}
#endif

#endif /* HA_ABI_H */
