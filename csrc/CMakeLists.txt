cmake_minimum_required(VERSION 3.10)
project(ha_codec VERSION 0.1.0 LANGUAGES C)

# ============================================================
# ha_codec — HomeAgent 内核编解码层（C 实现）
#
# 零外部依赖，纯 C99。产出静态库供 homed 经 cgo 链接，
# 同时可独立用于其他端（鸿蒙 / 嵌入式 / C SDK）。
#
# 使用方式:
#   add_subdirectory(path/to/csrc)
#   target_link_libraries(my_app ha_codec)
#   target_include_directories(my_app PRIVATE ${HA_CODEC_INCLUDE_DIR})
# ============================================================

option(BUILD_SHARED_LIBS "Build ha_codec as shared library" OFF)
option(BUILD_TESTS       "Build ha_codec tests" OFF)
option(BUILD_FUZZ       "Build libFuzzer targets" OFF)
option(BUILD_BENCH      "Build micro benchmarks" OFF)

# ★ C99 而非编译器默认档（clang 默认 gnu17）。
#   理由：内核 C 侧的编译契约是 C99（cgo CFLAGS 与这里必须一致），
#   在更新的默认档下编译会**静默**通过，而 Go 侧用 -std=c99 编不过
#   —— 两边同时构建、行为却分叉，是最难查的一类问题。
#   实测：本轮 -Wpedantic 门禁就当场抓到过 C11 特性（_Static_assert）。
#   C90/C95 不支持：ha_abi.h 用了 // 注释与 stdint。
#   C11 开关保留给想验证「未来切到 C11 也不坏」的人。
set(CMAKE_C_STANDARD 99)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)   # 禁用 gnu99 扩展，严格 -std=c99

set(HA_CODEC_SRC
    src/ha_codec.c
)

if(BUILD_SHARED_LIBS)
    add_library(ha_codec SHARED ${HA_CODEC_SRC})
    if(WIN32)
        set_target_properties(ha_codec PROPERTIES WINDOWS_EXPORT_ALL_SYMBOLS ON)
    endif()
else()
    add_library(ha_codec STATIC ${HA_CODEC_SRC})
endif()

set(HA_CODEC_INCLUDE ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_include_directories(ha_codec PUBLIC ${HA_CODEC_INCLUDE})

# 告警门禁：零告警才允许通过（与 Makefile 的 csrc-lint 同一标准）。
# C 侧没有 Go 的 vet 等价物，告警是唯一的静态信号。
if(CMAKE_C_COMPILER_ID MATCHES "GNU|Clang")
    add_compile_options(-Wall -Wextra -Wpedantic -Wshadow -Wconversion)
endif()

# 不链接任何外部库 —— 保持与 ha_remotedevice 同一克制标准
target_link_libraries(ha_codec PRIVATE)

set(HA_CODEC_INCLUDE_DIR ${HA_CODEC_INCLUDE} CACHE INTERNAL "ha_codec include directories")

install(TARGETS ha_codec
    EXPORT  ha_codec-targets
    LIBRARY DESTINATION lib
    ARCHIVE DESTINATION lib
    RUNTIME DESTINATION bin
    INCLUDES DESTINATION include
)
install(DIRECTORY include/ DESTINATION include)

# ============================================================
# 测试
# ============================================================
if(BUILD_TESTS)
    add_executable(ha_codec_test test/test_ha_codec.c)
    target_link_libraries(ha_codec_test PRIVATE ha_codec)
    enable_testing()
    add_test(NAME ha_codec_test COMMAND ha_codec_test)
endif()

# ============================================================
# 模糊测试：编码语义与内存安全的持续检验
#
# 动机：ha_codec 声称**逐值等价于 Go 参考实现**，其中最关键的一条是
#   「对畸形 UTF-8 的解码边界与 Go 的 utf8.DecodeRuneInString 一致」。
# 该行为在正常输入下永远测不到 —— 只有随机字节才能覆盖
#   截断序列 / 过长编码 / 代理对 / 超 U+10FFFF / 嵌入 NUL。
# Go 侧已有 TestGolden_InvalidUTF8（3000 组随机字节）做等价钉死；
# C 侧则需要独立验证两件事：
#   1. 任何输入都不崩、不越界（内存安全）
#   2. 返回值不违反头文件声明的不变式（0 <= keep <= len 等）
# 两者在 libFuzzer 上是持续的，而不是等下一次手写用例。
#
# 需 clang + -fsanitize=fuzzer；无则明确跳过（门禁不能假装通过）。
# ============================================================
if(BUILD_FUZZ)
    if(CMAKE_C_COMPILER_ID MATCHES "Clang")
        foreach(fz IN ITEMS test_fuzz_ha_codec)
            add_executable(${fz} test/${fz}.c)
            target_link_libraries(${fz} PRIVATE ha_codec)
            target_compile_options(${fz} PRIVATE
                -fsanitize=fuzzer,address,undefined
                -fno-omit-frame-pointer)
            target_link_options(${fz} PRIVATE
                -fsanitize=fuzzer,address,undefined)
        endforeach()
    else()
        message(WARNING
            "BUILD_FUZZ=ON 需要 clang（libFuzzer）；当前编译器是 "
            "${CMAKE_C_COMPILER_ID}，已跳过。")
    endif()
endif()

# ============================================================
# 微基准：C 侧自身的开销（与 Go 侧 codec_bench_test.go 对照）
#
# 为什么 C 侧也要基准：Go 侧基准里，「C 实现省下的时间」与「cgo 边界成本」
# 是混在一起的。若 C 侧本身在某场景很慢，改 Go 绑定无济于事；
# 必须在能隔离处（纯 C、无边界）测出函数体成本，才知道该优化谁。
# ============================================================
if(BUILD_BENCH)
    add_executable(ha_codec_bench bench/bench_ha_codec.c)
    target_link_libraries(ha_codec_bench PRIVATE ha_codec)
endif()
