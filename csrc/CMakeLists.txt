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
