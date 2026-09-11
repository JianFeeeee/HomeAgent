//go:build onnxruntime

package qwen3vl

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
)

const (
	qwenImageSize       = 768
	qwenPatchSize       = 16
	qwenTemporalPatch   = 2
	qwenSpatialMerge    = 2
	qwenImagePatches    = (qwenImageSize / qwenPatchSize) * (qwenImageSize / qwenPatchSize)
	qwenVisualTokens    = qwenImagePatches / (qwenSpatialMerge * qwenSpatialMerge)
	qwenPatchVectorSize = 3 * qwenTemporalPatch * qwenPatchSize * qwenPatchSize

	// qwenVisionScale 是视觉塔空间步长：grid_h / spatial_merge。
	// 每个时间组消耗这么多 M-RoPE 位置（见 model_input.go 的说明）。
	qwenVisionScale = (qwenImageSize / qwenPatchSize) / qwenSpatialMerge

	// maxVideoGroupsSafety 是分配安全上限，**不是**能力上限。
	// 真正能导出哪些档由产物目录决定（Vision_g{N}.onnx）；内核不硬编码
	// 导出清单，否则别人导出 G=8 就会被内核莫名拒绝。
	// 这个上限只用来防住「丢了上千帧进来」导致的巨量分配。
	maxVideoGroupsSafety = 64
)

// fitCanvas 把任意图片解码并转成固定 768×768 画布。
//
// 保持宽高比缩放并在中心补中性灰（归一化后约为 0）；直接强拉成正方形会
// 破坏物体形状。已是 768×768 的输入不做插值，以便用跨语言冻结向量
// 精确回归 patch 排列。
func fitCanvas(raw []byte) (*image.NRGBA, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("qwen: decode image: %w", err)
	}
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("qwen: empty image")
	}

	scale := math.Min(float64(qwenImageSize)/float64(b.Dx()), float64(qwenImageSize)/float64(b.Dy()))
	w := max(1, int(math.Round(float64(b.Dx())*scale)))
	h := max(1, int(math.Round(float64(b.Dy())*scale)))
	if w > qwenImageSize {
		w = qwenImageSize
	}
	if h > qwenImageSize {
		h = qwenImageSize
	}

	resized := resizeBicubic(src, w, h)
	canvas := image.NewNRGBA(image.Rect(0, 0, qwenImageSize, qwenImageSize))
	neutral := color.NRGBA{R: 128, G: 128, B: 128, A: 255}
	for i := 0; i < len(canvas.Pix); i += 4 {
		canvas.Pix[i], canvas.Pix[i+1], canvas.Pix[i+2], canvas.Pix[i+3] = neutral.R, neutral.G, neutral.B, neutral.A
	}
	ox, oy := (qwenImageSize-w)/2, (qwenImageSize-h)/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			canvas.SetNRGBA(ox+x, oy+y, resized.NRGBAAt(x, y))
		}
	}
	return canvas, nil
}

// appendPatches 按 Qwen2VLImageProcessor 的排列把一个时间组的两个画布写入 out。
//
// 排列：[grid_h/merge, grid_w/merge, merge_h, merge_w, channel,
// temporal_patch, patch_h, patch_w]，然后 flatten。
//
// 图像与视频共用本函数：图像的两个时间槽传同一张画布，视频传相邻两帧。
// 共用是刻意的——两处各写一份排列，迟早会在某次修改后漂移，
// 而排列错了只会得到一个语义偏移的向量，不会报错。
func appendPatches(out []float32, slots *[qwenTemporalPatch]*image.NRGBA) []float32 {
	blocks := qwenImageSize / qwenPatchSize / qwenSpatialMerge
	for bh := 0; bh < blocks; bh++ {
		for bw := 0; bw < blocks; bw++ {
			for mh := 0; mh < qwenSpatialMerge; mh++ {
				for mw := 0; mw < qwenSpatialMerge; mw++ {
					baseY := (bh*qwenSpatialMerge + mh) * qwenPatchSize
					baseX := (bw*qwenSpatialMerge + mw) * qwenPatchSize
					for c := 0; c < 3; c++ {
						for temporal := 0; temporal < qwenTemporalPatch; temporal++ {
							canvas := slots[temporal]
							for py := 0; py < qwenPatchSize; py++ {
								for px := 0; px < qwenPatchSize; px++ {
									p := canvas.NRGBAAt(baseX+px, baseY+py)
									v := [3]uint8{p.R, p.G, p.B}[c]
									out = append(out, float32(v)/127.5-1)
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// preprocessImage 把任意图片转成固定 768×768 视觉塔输入（单个时间组）。
func preprocessImage(raw []byte) ([]float32, error) {
	canvas, err := fitCanvas(raw)
	if err != nil {
		return nil, err
	}
	slots := [qwenTemporalPatch]*image.NRGBA{canvas, canvas}
	out := make([]float32, 0, qwenImagePatches*qwenPatchVectorSize)
	return appendPatches(out, &slots), nil
}

// preprocessVideoFrames 把已按时间排序的帧转成 G 个时间组的视觉塔输入，
// 返回 patch 张量与时间组数 G。
//
// 时间组 g 的两个时间槽依次取帧 2g 与 2g+1，这与处理器实测逐字节一致
// （纯色与异色两组对照均 torch.equal 通过）；布局整体是
// [G, blocks_h, blocks_w, merge_h, merge_w, c, temporal, patch_h, patch_w]，
// 即图像排列以 grid_t 为最外层堆叠。
//
// 帧数为奇数时不补帧：只用得上的帧参与编码，多余的一帧被丢弃，
// 以免用重复帧伪造时序——那会改变跨帧注意力看到的运动。
func preprocessVideoFrames(frames [][]byte) ([]float32, int, error) {
	if len(frames) < qwenTemporalPatch {
		return nil, 0, fmt.Errorf("qwen: video needs at least %d frames, got %d", qwenTemporalPatch, len(frames))
	}
	groups := len(frames) / qwenTemporalPatch
	if groups > maxVideoGroupsSafety {
		return nil, 0, fmt.Errorf("qwen: video groups %d exceeds safety limit %d（请先对帧采样）", groups, maxVideoGroupsSafety)
	}
	canvases := make([]*image.NRGBA, groups*qwenTemporalPatch)
	for i := 0; i < groups*qwenTemporalPatch; i++ {
		canvas, err := fitCanvas(frames[i])
		if err != nil {
			return nil, 0, fmt.Errorf("qwen: frame %d: %w", i, err)
		}
		canvases[i] = canvas
	}
	out := make([]float32, 0, groups*qwenImagePatches*qwenPatchVectorSize)
	for g := 0; g < groups; g++ {
		slots := [qwenTemporalPatch]*image.NRGBA{canvases[2*g], canvases[2*g+1]}
		out = appendPatches(out, &slots)
	}
	return out, groups, nil
}

// resizeBicubic 使用半像素中心的 Catmull-Rom 三次卷积。
func resizeBicubic(src image.Image, dstW, dstH int) *image.NRGBA {
	b := src.Bounds()
	if b.Dx() == dstW && b.Dy() == dstH {
		dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
		for y := 0; y < dstH; y++ {
			for x := 0; x < dstW; x++ {
				dst.SetNRGBA(x, y, color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA))
			}
		}
		return dst
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	sx, sy := float64(b.Dx())/float64(dstW), float64(b.Dy())/float64(dstH)
	for y := 0; y < dstH; y++ {
		fy := (float64(y)+0.5)*sy - 0.5
		y0 := int(math.Floor(fy))
		for x := 0; x < dstW; x++ {
			fx := (float64(x)+0.5)*sx - 0.5
			x0 := int(math.Floor(fx))
			var sum [4]float64
			var weight float64
			for j := -1; j <= 2; j++ {
				wy := cubicWeight(fy - float64(y0+j))
				yy := min(max(y0+j, 0), b.Dy()-1)
				for i := -1; i <= 2; i++ {
					w := wy * cubicWeight(fx-float64(x0+i))
					xx := min(max(x0+i, 0), b.Dx()-1)
					p := color.NRGBAModel.Convert(src.At(b.Min.X+xx, b.Min.Y+yy)).(color.NRGBA)
					sum[0] += float64(p.R) * w
					sum[1] += float64(p.G) * w
					sum[2] += float64(p.B) * w
					sum[3] += float64(p.A) * w
					weight += w
				}
			}
			if weight == 0 {
				weight = 1
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: clampByte(sum[0] / weight), G: clampByte(sum[1] / weight),
				B: clampByte(sum[2] / weight), A: clampByte(sum[3] / weight),
			})
		}
	}
	return dst
}

func cubicWeight(x float64) float64 {
	x = math.Abs(x)
	if x <= 1 {
		return 1.5*x*x*x - 2.5*x*x + 1
	}
	if x < 2 {
		return -0.5*x*x*x + 2.5*x*x - 4*x + 2
	}
	return 0
}

func clampByte(v float64) uint8 {
	return uint8(min(255, max(0, int(math.Round(v)))))
}
