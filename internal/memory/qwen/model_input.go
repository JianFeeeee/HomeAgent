//go:build onnxruntime

package qwen

import (
	"fmt"
	"strings"
)

// imageModelInput 构造 Qwen3-VL 单图对话模板及对应 M-RoPE 位置。
// 固定 768×768 视觉塔产生 576 个合并后的视觉 token。
func (t *Tokenizer) imageModelInput(instruction string, maxLen int) (ids []int, attention, position []int64, visual []bool, err error) {
	if instruction == "" {
		instruction = DefaultInstruction
	}
	text := "<|im_start|>system\n" + instruction +
		"<|im_end|>\n<|im_start|>user\n<|vision_start|>" +
		strings.Repeat("<|image_pad|>", qwenVisualTokens) +
		"<|vision_end|><|im_end|>\n<|im_start|>assistant\n"
	ids, err = t.encodeModelInput(text, maxLen)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	imageID, ok := t.SpecialID("<|image_pad|>")
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("tokenizer.json 缺少 <|image_pad|>")
	}

	visual = make([]bool, len(ids))
	attention = make([]int64, len(ids))
	position = make([]int64, 3*len(ids))
	for i, id := range ids {
		attention[i] = 1
		visual[i] = id == imageID
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
			if end-start != qwenVisualTokens {
				return nil, nil, nil, nil, fmt.Errorf("qwen: image token count=%d, want %d", end-start, qwenVisualTokens)
			}
			side := qwenImageSize / qwenPatchSize / qwenSpatialMerge
			for i := start; i < end; i++ {
				j := i - start
				position[i] = current
				position[len(ids)+i] = current + int64(j/side)
				position[2*len(ids)+i] = current + int64(j%side)
			}
			current += int64(side)
		}
		start = end
	}
	return ids, attention, position, visual, nil
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
