cmake_minimum_required(VERSION 3.10)
project(ha_remotedevice VERSION 0.1.0 LANGUAGES C)

# ============================================================
# ha_remotedevice — HomeAgent 远程设备接入 C SDK
# 零外部依赖，纯 C 实现，兼容嵌入式平台。
#
# 使用方式:
#   add_subdirectory(path/to/ha_remotedevice)
#   target_link_libraries(my_app ha_remotedevice)
#   target_include_directories(my_app PRIVATE
#     ${HA_REMOTEDEVICE_INCLUDE_DIR})
# ============================================================

# 选项: 构建为静态库或动态库
option(BUILD_SHARED_LIBS "Build ha_remotedevice as shared library" OFF)

# 选项: 禁用 malloc/free（用于裸机环境，用户需提供 alloc 回调）
option(HA_NO_ALLOC  "Disable dynamic memory allocation" OFF)

# 选项: 日志级别
set(HA_LOG_LEVEL 2 CACHE STRING "Log level: 0=none, 1=error, 2=info, 3=debug")

# 源文件
set(HA_REMOTEDEVICE_SRC
    src/ha_remotedevice.c
    src/ha_json.c
    src/ha_ws.c
)

# 头文件
set(HA_REMOTEDEVICE_INCLUDE
    ${CMAKE_CURRENT_SOURCE_DIR}/include
)

# 编译选项
if(HA_NO_ALLOC)
    add_definitions(-DHA_NO_ALLOC)
endif()
add_definitions(-DHA_LOG_LEVEL=${HA_LOG_LEVEL})

# 创建库
if(BUILD_SHARED_LIBS)
    add_library(ha_remotedevice SHARED ${HA_REMOTEDEVICE_SRC})
    if(WIN32)
        # Windows 需要导出符号
        set_target_properties(ha_remotedevice PROPERTIES
            WINDOWS_EXPORT_ALL_SYMBOLS ON)
    endif()
else()
    add_library(ha_remotedevice STATIC ${HA_REMOTEDEVICE_SRC})
endif()

# 包含目录
target_include_directories(ha_remotedevice
    PUBLIC  ${HA_REMOTEDEVICE_INCLUDE}
    PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/src
)

# 不链接任何外部库
target_link_libraries(ha_remotedevice PRIVATE)

# 导出包含目录供外部项目使用
set(HA_REMOTEDEVICE_INCLUDE_DIR
    ${HA_REMOTEDEVICE_INCLUDE}
    CACHE INTERNAL "ha_remotedevice include directories")

# 安装规则
install(TARGETS ha_remotedevice
    EXPORT  ha_remotedevice-targets
    LIBRARY DESTINATION lib
    ARCHIVE DESTINATION lib
    RUNTIME DESTINATION bin
    INCLUDES DESTINATION include
)

install(DIRECTORY include/
    DESTINATION include
)

install(EXPORT ha_remotedevice-targets
    DESTINATION lib/cmake/ha_remotedevice
    NAMESPACE ha_remotedevice::
)

# ============================================================
# 测试（可选）
# ============================================================
option(BUILD_TESTS "Build ha_remotedevice tests" OFF)

if(BUILD_TESTS)
    find_package(Threads REQUIRED)

    add_executable(ha_remotedevice_test
        test/test_ha_remotedevice.c
    )
    target_link_libraries(ha_remotedevice_test
        PRIVATE ha_remotedevice Threads::Threads
    )
    target_include_directories(ha_remotedevice_test
        PRIVATE ${HA_REMOTEDEVICE_INCLUDE_DIR}
    )

    # 添加测试
    add_test(NAME ha_remotedevice_test
        COMMAND ha_remotedevice_test
    )
endif()

# ============================================================
# 编译信息
# ============================================================
message(STATUS "ha_remotedevice ${PROJECT_VERSION}")
message(STATUS "  Build type: $<CONFIG>")
message(STATUS "  Shared lib: ${BUILD_SHARED_LIBS}")
message(STATUS "  No alloc:   ${HA_NO_ALLOC}")