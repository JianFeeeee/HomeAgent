# QQ 通知插件
version: 1.0.0
author: HomeAgent
io_type: io
io_input_route: qq
io_output_route: qq
io_output_caps: text,file,image

QQ 通道插件，通过 OneBot 标准协议与 QQ 前端（go-cqhttp / Lagrange）协同工作。
OneBot 前端处理 QQ 协议登录与消息收发，本插件将 OneBot 事件转换为 IO 输入，
将 AI 输出转换为 OneBot 动作调用。

## qq_send_private_msg
发送 QQ 私聊消息
- user_id: 目标 QQ 号
- message: 消息内容（支持 CQ 码，如 [CQ:image,file=xxx.jpg]）
- auto_escape: 是否作为纯文本发送

## qq_send_group_msg
发送 QQ 群消息
- group_id: 目标群号
- message: 消息内容（支持 CQ 码）
- auto_escape: 是否作为纯文本发送

## qq_get_group_member_info
获取 QQ 群成员信息
- group_id: 群号
- user_id: QQ 号

## qq_get_group_list
获取 QQ 群列表
