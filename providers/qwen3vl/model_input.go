//go:build onnxruntime

package qwen3vl

import (
	"fmt"
	"strings"
)

// 视觉输入（图像与视频）的模型输入构造。
//
// 图像与视频的模板结构完全一致，只有两点不同：
//   1. 占位符：<|image_pad|>（id 151655）vs <|video_pad|>（id 151656）；
//   2. 时间组数：图像恒为 1 组（576 个视觉 token），视频为 G 组（G×576）。
//
// 因此两者共用同一个构造器。分开写两份必然漂移，而漂移的表现是
// 「嵌入略有不同」——不报错，只是检索慢慢变差。

// visionModelInput 构造视觉输入的 token 序列与 M-RoPE 位置。
//
// padToken 是 <|image_pad|> 或 <|video_pad|>；groups 是时间组数。
func (t *Tokenizer) visionModelInput(instruction, padToken string, groups, maxLen int) (ids []int, attention, position []int64, visual []bool, err error) {
	if instruction == "" {
		instruction = DefaultInstruction
	}
	if groups < 1 {
		return nil, nil, nil, nil, fmt.Errorf("qwen: vision groups must be >= 1, got %d", groups)
	}
	if _, ok := t.SpecialID(padToken); !ok {
		return nil, nil, nil, nil, fmt.Errorf("tokenizer.json 缺少 %s", padToken)
	}
	text := "<|im_start|>system\n" + instruction +
		"<|im_end|>\n<|im_start|>user\n<|vision_start|>" +
		strings.Repeat(padToken, groups*qwenVisualTokens) +
		"<|vision_end|><|im_end|>\n<|im_start|>assistant\n"
	ids, err = t.encodeModelInput(text, maxLen)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	padID, _ := t.SpecialID(padToken)

	visual = make([]bool, len(ids))
	attention = make([]int64, len(ids))
	position = make([]int64, 3*len(ids))
	for i, id := range ids {
		attention[i] = 1
		visual[i] = id == padID
	}

	current := int64(0)
	for start := 0; start < len(ids); {
		isVisual := visual[start]
		end := start + 1
		for end < len(ids) && visual[end] == isVisual {
			end++
		}
		if !isVisual {
			for i := start; i < end; i++ {
				p := current + int64(i-start)
				position[i] = p
				position[len(ids)+i] = p
				position[2*len(ids)+i] = p
			}
			current += int64(end - start)
		} else {
			run := end - start
			if run != groups*qwenVisualTokens {
				return nil, nil, nil, nil, fmt.Errorf("qwen: %s run=%d, want %d (groups=%d)",
					padToken, run, groups*qwenVisualTokens, groups)
			}
			// 每个时间组独立取位置：t 在组内固定为 base，h/w 在组内递增，
			// 组间 base 前进一个视觉步长。
			//
			// 与 transformers 的实现对应：get_rope_index 对视频先把
			// video_grid_thw 按 grid_t 展开成 G 个 (1,h,w) 的 grid 项，
			// 每项单独调用 get_vision_position_ids(current_pos, (1,h,w))，
			// 然后 current_pos += max(h,w)/spatial_merge。因为每项 t=1，
			// 其 temporal 分量就等于 current_pos，h/w 从 current_pos 起递增。
			for g := 0; g < groups; g++ {
				base := current
				for j := 0; j < qwenVisualTokens; j++ {
					i := start + g*qwenVisualTokens + j
					position[i] = base
					position[len(ids)+i] = base + int64(j/qwenVisionScale)
					position[2*len(ids)+i] = base + int64(j%qwenVisionScale)
				}
				current += int64(qwenVisionScale)
			}
		}
		start = end
	}
	return ids, attention, position, visual, nil
}

// imageModelInput 构造 Qwen3-VL 单图对话模板及对应 M-RoPE 位置。
// 固定 768×768 视觉塔产生 576 个合并后的视觉 token。
func (t *Tokenizer) imageModelInput(instruction string, maxLen int) (ids []int, attention, position []int64, visual []bool, err error) {
	return t.visionModelInput(instruction, "<|image_pad|>", 1, maxLen)
}

// videoModelInput 构造 Qwen3-VL 视频对话模板及对应 M-RoPE 位置。
//
// groups 是时间组数（每组合 2 帧），共 2×groups 帧、groups×576 个视觉 token。
// 占位符是 <|video_pad|>（id 151656），与图像的 <|image_pad|> 不同——
// 用错占位符不会报错，只会让模型把它当成另一种模态。
//
// 这里不限制 groups 上限：哪些档位真的可用由产物目录（Vision_g{N}.onnx）决定，
// 硬编码一份清单在这里只会与导出脚本漂移。序列过长会因 tokenizer 截断
// 而在下面的视觉区间长度校验处明确报错。
func (t *Tokenizer) videoModelInput(instruction string, groups, maxLen int) (ids []int, attention, position []int64, visual []bool, err error) {
	return t.visionModelInput(instruction, "<|video_pad|>", groups, maxLen)
}

// textModelInput 执行完整 tokenizer post_processor，并构造纯文本标准 RoPE 位置。
func (t *Tokenizer) textModelInput(instruction, text string, maxLen int) (ids []int, attention, position []int64, visual []bool, err error) {
	ids, err = t.encodeModelInput(renderInstructionInput(instruction, text), maxLen)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	attention = make([]int64, len(ids))
	position = make([]int64, 3*len(ids))
	visual = make([]bool, len(ids))
	for i := range ids {
		attention[i] = 1
		position[i] = int64(i)
		position[len(ids)+i] = int64(i)
		position[2*len(ids)+i] = int64(i)
	}
	return ids, attention, position, visual, nil
}
