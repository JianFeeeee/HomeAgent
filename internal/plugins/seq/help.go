package seq

// 本文件提供 seq_help 的文本：格式说明 + 可照抄的完整示例。
//
// 为什么要有它（真机实跑的直接动机）：模型写序列时踩了三个坑，各试了
// 1~3 次才改对 ——
//  ① tools 漏末尾的 ';'        → 「末尾缺少 ';'」
//  ② group 的 in 传成字符串     → 重试 3 次
//  ③ as 指向未声明的 out 槽      → 静态校验拦下
// 这三处都是**格式细节**，塞不进工具描述（有长度限制），却恰恰是模型最容易
// 错的地方。散落在六个描述里等于没有集中入口。
//
// ⚠️ 下面的示例**由判据校验其自身能被 Parse 接受**
// （plugin_test.go: TestSeqHelpIncludesCopyableExample）——
// 模型是照抄的，示例自己解析不过就是给模型挖坑。

// seqHelpText 返回帮助文本。
func seqHelpText() string {
	return helpHeader + helpFormat + helpCondition + helpExample
}

const helpHeader = `【工具序列 seq】

⚠️ 最容易错的一处（真机实测模型在此连续失败 4 次）：

     group 的 in 必须是**对象**，无入参写 {}   ——   不要写成字符串

  "in": "" 或 "in": "{}"（字符串）一律被拒。「无入参」要表达成空**对象**。

常用操作：
  seq_list                    列出全部序列及其签名
  seq_create                  新建/更新（groups 传参 或 file 加载，二选一）
  seq_run                     执行（按 groups 数组顺序逐组跑）
  seq_call                    按名调用某个 group 或某条序列
  seq_when_call               条件调用，when 为真才执行
  seq_delete                  删除

序列 = 若干 group，**组内并行、组间串行**；每个 group 有独立签名
（in 入参 / out 出参），可被 seq_call 按名调用。
序列存的是**解析后的 AST**：保存时做完全部静态校验，执行期不再解析文本。
`

const helpFormat = `
【格式要点 —— 这几处最容易错】

1. 顶层是 JSON：{"name":…, "description":…, "groups":[…]}，groups 至少一个。

2. 每个 group 的字段：
     name       必填，组名，全局唯一（它是签名名）
     in         入参声明，**对象**，如 {"host":"string"}；无入参写 {}
                ⚠️ 必须是对象 {"k":"type"}；写 "" 或 "{}"（字符串）一律被拒
     out        出参声明，**对象**，如 {"summary":"string"}；无出参写 {}
     when       条件屏障，默认 "true"，只可读 $args.*
     parallel   默认 true；置 false 则组内串行（保序场景用）
     missing    工具不存在时的行为：fail（默认）/ skip / degrade
     timeout    本组墙钟上限，如 "30s"
     on_error   abort（默认）/ continue / retry
     tools      **字符串**（不是数组！），见下

3. ⚠️ tools 是**字符串**，内部是若干以 ';' 分隔的 JSON 对象：
     每个对象形如 {"tool":"cmd_run","args":{…},"as":"槽名"}
     - 每个 tool 后**必须**跟 ';'，**包括最后一个**。漏了报「末尾缺少 ';'」。
     - 相邻两个 tool 之间也要有 ';'。漏了会让下一个工具被**静默吞掉**。
     - 键必须带引号（是合法 JSON）：{"tool":…} 而不是 {tool:…}。
     - args 里可用 $args.<键> 引用入参；整值引用保留类型。

4. ⚠️ as 写的槽名**必须已在 out 里声明**，否则保存时报错。
   非 array 的槽被同名 as 写多次也报错（组内并行会数据竞争）；
   需要累加就把该槽声明成 "array"。

5. groups 与 file **二选一**：短序列用 groups 直接传；长序列写文件后用 file
   传路径（长参数会被 max_tokens 截断，写文件更稳）。
`

const helpCondition = `
【条件 when】

只可读本组的 $args.*（即 in 里声明过的入参），不能读别组的出参。
求值失败会**报错**（不会静默当成假），因为静默跳过会让序列少做一步而你以为跑完了。
  "$args.flag == true"     布尔比较
  "$args.n > 3"            数值比较
  "$args.s contains \"err\""  文本包含
  "$args.host != \"\""        非空判断
  "true" / "false"          恒真/恒假
`

const helpExample = `
【可照抄的完整示例】

下面这一行是**完整合法**的序列（单行紧凑，直接照抄即可）：

{"name": "巡检三节点", "description": "并行拉取三台节点状态，异常时展开", "groups": [{"name": "拉取单台", "description": "拉取一台节点的 uptime 与负载", "in": {"host": "string"}, "out": {"summary": "string", "load": "string"}, "tools": "{\"tool\":\"cmd_run\",\"args\":{\"command\":\"uptime\"},\"as\":\"summary\"} ; {\"tool\":\"cmd_run\",\"args\":{\"command\":\"date\"},\"as\":\"load\"} ;"}, {"name": "异常展开", "in": {"host": "string"}, "out": {"detail": "string"}, "when": "$args.host != \"\"", "tools": "{\"tool\":\"cmd_run\",\"args\":{\"command\":\"journalctl -x\"},\"as\":\"detail\"} ;"}]}

拆开看（仅为阅读方便，实际照抄上面那一行）：

  name / description        序列名与说明
  groups[0]  拉取单台        组内两个工具**并行**（都声明了才并发，否则整批串行）
              in  {"host":"string"}     ← 对象，不是字符串
              out {"summary":…,"load":…} ← as 要写的槽必须在这里声明
              tools 字符串里两个 {...} 之间、以及**最后一个之后**，都有 ';'
  groups[1]  异常展开        when 条件：只读本组 in 声明过的 $args.*

对应调用：
  seq_create(name="巡检三节点", groups=[上面那个数组])
  seq_run(name="巡检三节点", args={"host":"node-a"})

⚠️ 这段示例由判据校验其**自身能被解析器接受**（model 照抄不会踩坑）。
`
