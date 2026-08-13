package core

import (
	"fmt"
	"strings"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

func (a *Agent) buildMemoryContext(input string, maxTokens int) string {
	if a.indexer == nil {
		return ""
	}
	injected := a.indexer.BuildContext(input)
	s := a.indexer.FormatContext(injected)
	if maxTokens > 0 {
		s = TruncateByTokens(s, maxTokens)
	}
	return s
}

func (a *Agent) buildSystemPrompt(memContext string, userInput string) string {
	prompt := a.systemPrompt
	if prompt == "" {
		prompt = "你是小宅，HomeAgent 的看板娘，一个家政型 AI 管家助手。绝不用 Unicode emoji，只用颜文字表达情感，句尾带语气词。WebUI 概览页展示你的立绘。"
	}

	if a.personality != nil {
		if pp := a.personality.InjectPrompt(); pp != "" {
			prompt += "\n\n" + pp
		}
	}

	if memContext != "" {
		prompt += "\n\n" + memContext
	}

	prompt += "\n\n【记忆清理指令】当用户要求整理或清理记忆时，你必须实际调用 memory_ 工具执行操作，不能只回复文本。先用 memory_introspect 查看概况，再用 memory_recall 获取详情。有同义实体则用 memory_merge 合并（source 会被彻底删除），有无用噪音实体则用 memory_delete_entity 直接删除，也可用 memory_purge 批量清理，用 memory_edit 修正错误，用 memory_block_merge 标记不合并。如果工具执行成功，把结果告知用户；不要只描述计划而不执行。"

	if a.docStore != nil {
		docs := a.docStore.Query(userInput, 3)
		if len(docs) > 0 {
			var parts []string
			parts = append(parts, "【相关记忆文档】")
			for i, d := range docs {
				parts = append(parts, fmt.Sprintf("  [%d] %s", i+1, d.Summary))
			}
			prompt += "\n\n" + strings.Join(parts, "\n")
		}
	}

	prompt += "\n\n【中断消息】长任务执行期间，工具/插件/定时器等会通过中断机制向你发送提醒（如 QQ 新消息、终端输出到达、定时器到点等）。中断消息以 system 角色注入，内容带 [中断消息] 前缀，**不是用户发言，但也必须认真处理**：优先停下当前长任务，针对中断内容作出响应或决定继续执行。不要忽略带 [中断消息] 前缀的 system 消息。"

	prompt += "\n\n【输出规则】回复会自动发送到用户的输入来源通道，直接返回纯文本即可送达，无需调用任何工具。\n"
	prompt += "- 输出门工具 output_send__{通道名} 用于主动向指定通道推送消息（如群发、主动通知、向其他通道发言），不是回复的必要步骤。除非用户要求在别的通道发送，否则不要使用。\n"
	prompt += "- 用 output_send__{通道名}_help 查看该通道的 meta 格式和 type 枚举。\n"
	prompt += "- 同一轮对话中可多次调用输出门工具。长消息应当分多次发出，而不是一口气发完。\n"
	prompt += "- 需要多步执行的长任务：**必须先**用输出门工具向当前输入通道发一条确认消息告诉用户已收到（如「好的我去看看～」，也可以直接返回文本），**然后再**执行具体排查工具。确认消息不代表任务完成，发出后仍需继续执行实际工具并最终汇报结果。"

	if a.indexer != nil {
		prompt += "\n\n" + a.indexer.BuildToolPrompt()
	}

	prompt += a.buildToolCatalog()

	return prompt
}

func cleanParams(params map[string]interface{}) map[string]interface{} {
	if params == nil {
		return nil
	}
	cleaned := make(map[string]interface{}, len(params))
	for k, v := range params {
		cleaned[k] = v
	}
	if req, ok := cleaned["required"]; ok {
		switch v := req.(type) {
		case []interface{}:
			if len(v) == 0 {
				delete(cleaned, "required")
			}
		case []string:
			if len(v) == 0 {
				delete(cleaned, "required")
			}
		}
	}
	return cleaned
}

func (a *Agent) buildToolCatalog() string {
	defs := a.buildToolDefs()
	if len(defs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n【可用工具列表】")
	seen := make(map[string]bool)
	for _, d := range defs {
		t, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		desc, _ := fn["description"].(string)
		sb.WriteString(fmt.Sprintf("\n- %s", name))
		if desc != "" {
			if len(desc) > 80 {
				desc = desc[:80] + "..."
			}
			sb.WriteString(": " + desc)
		}
	}
	return sb.String()
}

func (a *Agent) buildToolDefs() []interface{} {
	var tools []interface{}

	if a.io != nil {
		for _, td := range a.io.GetAllTools() {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  cleanParams(td.Parameters),
				},
			})
		}
	}

	if a.stageHost != nil {
		for _, td := range a.stageHost.GetToolDefs() {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  cleanParams(td.Parameters),
				},
			})
		}
	}

	if a.indexer != nil {
		for _, td := range a.indexer.GetToolDefinitions() {
			tools = append(tools, td)
		}
	}

	if a.memory != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_merge",
				"description": "【记忆清理】合并两个同义实体。将所有关系从 source 重定向到 target，然后彻底删除 source。注意：实体删除后不可恢复，合并前请确认语义一致。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"source": map[string]interface{}{"type": "string", "description": "被合并的实体名（合并后消失）"},
						"target": map[string]interface{}{"type": "string", "description": "保留的实体名"},
					},
					"required": []string{"source", "target"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_delete_entity",
				"description": "【记忆清理】彻底删除指定实体及其所有关联关系。用于清理无用的噪音实体，如 mentionCount=0 的孤立实体、distiller 自动产生的垃圾节点、确认无用的旧数据。此操作不可恢复。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{"type": "string", "description": "要删除的实体名称"},
					},
					"required": []string{"name"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_block_merge",
				"description": "【记忆清理】标记两个实体在指定轮次内不尝试合并，用于阻止误判。当 LLM 判断两个实体虽然相似但不是同一事物时，使用此工具阻止后续心跳自动推送合并候选。每次心跳扫描双方计数各减一，归零后恢复候选资格。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"entity_a": map[string]interface{}{"type": "string", "description": "第一个实体名"},
						"entity_b": map[string]interface{}{"type": "string", "description": "第二个实体名"},
						"rounds":   map[string]interface{}{"type": "integer", "description": "阻止轮次数（每次心跳各减一，归零后恢复）"},
					},
					"required": []string{"entity_a", "entity_b", "rounds"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_purge",
				"description": "【记忆清理】删除记忆库中符合条件的垃圾关系和数据。当用户要求整理记忆时，用 memory_introspect 发现低质量实体后，用此工具批量删除。如 @merged 后缀的残留实体、mentionCount=0 的孤立实体、distiller 自动生成的噪音关系等。支持软删（soft）和物理删除（hard）。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"subject_contains": map[string]interface{}{"type": "string", "description": "主体名包含的关键词，如 '@merged' 可清理已合并残留"},
						"relation_type":    map[string]interface{}{"type": "string", "description": "关系类型，如 '提及'、'回应'"},
						"target_contains":  map[string]interface{}{"type": "string", "description": "客体名包含的关键词"},
						"mode":             map[string]interface{}{"type": "string", "description": "soft（标记删除）/ hard（物理删除）", "default": "soft"},
					},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_edit",
				"description": "【记忆清理】编辑单条记忆关系：删除旧的 relation 并写入新的。用于修正错误的实体名或关系类型。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"old_subject":  map[string]interface{}{"type": "string", "description": "旧主体名"},
						"old_relation": map[string]interface{}{"type": "string", "description": "旧关系类型"},
						"old_object":   map[string]interface{}{"type": "string", "description": "旧客体名"},
						"new_subject":  map[string]interface{}{"type": "string", "description": "新主体名（不填则不变）"},
						"new_relation": map[string]interface{}{"type": "string", "description": "新关系类型（不填则不变）"},
						"new_object":   map[string]interface{}{"type": "string", "description": "新客体名（不填则不变）"},
					},
					"required": []string{"old_subject", "old_relation", "old_object"},
				},
			},
		})
	}

	if a.knowledge != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_search",
				"description": "搜索知识库。输入查询关键词，返回相关知识内容。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string", "description": "查询关键词"},
						"top_k": map[string]interface{}{"type": "integer", "description": "返回数量", "default": 5},
					},
					"required": []string{"query"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_list",
				"description": "列出知识库中所有知识分类。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
	}

	if a.knowledge != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_create",
				"description": "创建新知识。将知识写入知识库（knowledge/目录），自动向量化索引。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{"type": "string", "description": "知识名称（用作目录名）"},
						"content": map[string]interface{}{"type": "string", "description": "知识内容，支持 Markdown"},
					},
					"required": []string{"name", "content"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_delete",
				"description": "删除知识库中的指定知识条目。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{"type": "string", "description": "要删除的知识名称"},
					},
					"required": []string{"name"},
				},
			},
		})
	}

	if a.docStore != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "doc_query",
				"description": "查询文档记忆。输入查询内容，返回相关文档摘要。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string", "description": "查询内容"},
						"top_k": map[string]interface{}{"type": "integer", "description": "返回数量", "default": 3},
					},
					"required": []string{"query"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "doc_commit",
				"description": "提交一条文档记忆。将重要信息显式写入文档记忆层。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{"type": "string", "description": "文档内容"},
						"summary": map[string]interface{}{"type": "string", "description": "摘要（可选）"},
						"tags": map[string]interface{}{
							"type":        "array",
							"description": "标签列表",
							"items":       map[string]interface{}{"type": "string"},
						},
					},
					"required": []string{"content"},
				},
			},
		})
	}

	if a.social != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_query",
				"description": "查询指定人物的完整档案（特质+社交关系）。用于了解一个人的性格、喜好、背景和社交圈。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{"type": "string", "description": "人物名称"},
					},
					"required": []string{"name"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_set_trait",
				"description": "记录/更新一个人的特质（性格、喜好、习惯等）。例如：person_set_trait(name=\"张三\", trait=\"喜欢\", value=\"红色\")。如果该特质已存在则覆盖。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":  map[string]interface{}{"type": "string", "description": "人物名称"},
						"trait": map[string]interface{}{"type": "string", "description": "特质名称，如：喜欢、性格、职业、年龄"},
						"value": map[string]interface{}{"type": "string", "description": "特质值，如：红色、开朗、工程师、25岁"},
					},
					"required": []string{"name", "trait", "value"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_relate",
				"description": "记录两个人之间的社交关系。例如：person_relate(person_a=\"张三\", relation=\"朋友\", person_b=\"李四\")。关系是双向的。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"person_a": map[string]interface{}{"type": "string", "description": "人物A"},
						"relation": map[string]interface{}{"type": "string", "description": "关系类型，如：朋友、家人、同事、邻居、同学"},
						"person_b": map[string]interface{}{"type": "string", "description": "人物B"},
					},
					"required": []string{"person_a", "relation", "person_b"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_network",
				"description": "查询某人的社交网络（多度关系）。显示该人物周围的相关人物及其关系和特质。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":  map[string]interface{}{"type": "string", "description": "人物名称"},
						"depth": map[string]interface{}{"type": "integer", "description": "关系深度（默认2）", "default": 2},
					},
					"required": []string{"name"},
				},
			},
		})
	}

	if a.pluginReg != nil && a.pluginDir != "" {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "plgreload",
				"description": "重载 plugins/ 目录的所有插件。扫描目录变更，原子化替换 IO 设备。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
	}

	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "spawn_child",
			"description": "启动一个异步子 Agent 执行独立任务。子 Agent 后台运行，不阻塞当前对话。完成后系统会自动通知你，届时请调用 child_result 工具查看输出。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task": map[string]interface{}{
						"type":        "string",
						"description": "要子 Agent 完成的任务描述。请描述清晰、完整，包含所有必要背景。",
					},
				},
				"required": []string{"task"},
			},
		},
	})
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "child_result",
			"description": "查询异步子 Agent 的执行结果。当收到'子任务已完成'的通知后，调用此工具获取输出。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "string",
						"description": "spawn_child 返回的任务 ID，如 child_1",
					},
				},
				"required": []string{"task_id"},
			},
		},
	})

	if a.providerManager != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "llm_list_sources",
				"description": "列出所有可用的 LLM 源（如 deepseek、openai、ollama），每个源有对应的 Lua 适配器和配置。如需切换 LLM 源，请使用 llm_set_source。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "llm_set_source",
				"description": "切换当前 LLM 源到指定名称。变更立即生效，后续对话将使用新的 LLM 源。源名称可通过 llm_list_sources 查看。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type":        "string",
							"description": "LLM 源名称（如 deepseek、openai、ollama）",
						},
					},
					"required": []string{"name"},
				},
			},
		})
	}

	channels := a.io.ListChannels()
	for _, ch := range channels {
		if ch.Type != agentIO.DeviceOutput && ch.Type != agentIO.DeviceIO {
			continue
		}
		capStr := a.io.GetChannelCapabilities(ch.Name).String()
		desc := ch.Description
		if desc == "" {
			desc = ch.Name + " 输出通道"
		}

		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "output_send__" + ch.Name,
				"description": desc + "。能力: " + capStr + "。payload 为消息载荷，meta 为 JSON 发送元数据，type 为载荷类型。用 _help 查看 meta 格式和 type 枚举。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"payload": map[string]interface{}{
							"type":        "string",
							"description": "消息载荷。type=text 时填文字，type=file/image 时填 URL 或路径",
						},
						"meta": map[string]interface{}{
							"type":        "string",
							"description": "JSON 对象，包含发送所需的元数据。用 output_send__" + ch.Name + "_help 查看 meta 格式",
						},
						"type": map[string]interface{}{
							"type":        "string",
							"description": "载荷类型，用 channel._help 查看支持的枚举值",
						},
					},
					"required": []string{"payload", "type"},
				},
			},
		})

		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "output_send__" + ch.Name + "_help",
				"description": "查看 " + ch.Name + " 输出通道的 meta 格式说明和 type 枚举",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
	}

	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "output_list_channels",
			"description": "列出所有可用输出通道及其能力（如 text/file/image/audio）和对应的输出门工具名称。",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	})

	if a.pendingMedia != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "describe_image",
				"description": "描述当前用户上传的图片内容。使用配置的多模态模型或默认 LLM 进行识别。调用此工具后你将获得图片的详细文字描述。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"provider": map[string]interface{}{
							"type":        "string",
							"description": "可选：用于图片描述的 LLM 源名称，不填则使用默认模型",
						},
						"detail": map[string]interface{}{
							"type":        "string",
							"description": "描述详细程度: high / low / auto",
							"default":     "high",
						},
					},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "transcribe_audio",
				"description": "转写当前用户上传的音频内容为文字。使用配置的多模态模型或默认 LLM 进行语音识别。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"provider": map[string]interface{}{
							"type":        "string",
							"description": "可选：用于音频转写的 LLM 源名称，不填则使用默认模型",
						},
					},
				},
			},
		})
		if a.inputCfg.Image.OCREnabled {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "ocr_image",
					"description": "对当前用户上传的图片执行 OCR 文字识别，提取图片中的文字内容。适用于截图、文档照片、菜单等场景。",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"language": map[string]interface{}{
								"type":        "string",
								"description": "OCR 语言（如 chi_sim+eng），默认自动",
							},
						},
					},
				},
			})
		}
	}

	return tools
}
