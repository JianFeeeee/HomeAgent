#!/bin/bash
# 在**私有 netns** 里启动：mock LLM + 完整内核实例。
# 私有 netns 的意义：生产实例占着 *:8080 / *:9890 / *:9876，插件设了 SO_REUSEADDR，
# 同机再起一个实例会在 127.0.0.1 上与之并存绑定（第一次实测就这么抢到了 9890 约 1 分钟）。
# netns 里只有 lo，结构上不可能碰到生产端口。
set -e
DATA=/var/tmp/gotmp/kstress
DRIVE=/var/tmp/gotmp/kstress-drive
DELAY=${MOCK_DELAY_MS:-300}
CHUNKS=${MOCK_CHUNKS:-8}
unshare -n bash -c "
ip link set lo up
nohup env MOCK_DELAY_MS=$DELAY MOCK_CHUNKS=$CHUNKS python3 $DRIVE/mockllm.py > $DATA/mock.log 2>&1 &
echo \$! > $DATA/mock.pid
nohup $1 -data $DATA -webui 127.0.0.1:18080 > $DATA/boot.log 2>&1 &
echo \$! > $DATA/pid
"
