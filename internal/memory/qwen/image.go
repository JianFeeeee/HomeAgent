//go:build onnxruntime

package qwen

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
)

// preprocessImage 把任意图片转成固定 768×768 视觉塔输入。
//
// Vision.onnx 是经过 PyTorch 逐输出验证的固定 48×48 patch 图。为避免拉伸物体，
// 这里保持宽高比缩放并在中心补中性灰（归一化后约为 0）；这与直接把长方形
// 强拉成正方形相比更能保留 Qwen 的视觉语义。已是 768×768 的输入不做插值，
// 便于用跨语言冻结向量精确回归 patch 排列。
func preprocessImage(raw []byte) ([]float32, error) {
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

	// 与 transformers Qwen2VLImageProcessor 的排列严格一致：
	// [grid_h/merge, grid_w/merge, merge_h, merge_w, channel,
	//  temporal_patch, patch_h, patch_w]，然后 flatten。
	out := make([]float32, 0, qwenImagePatches*qwenPatchVectorSize)
	blocks := qwenImageSize / qwenPatchSize / qwenSpatialMerge
	for bh := 0; bh < blocks; bh++ {
		for bw := 0; bw < blocks; bw++ {
			for mh := 0; mh < qwenSpatialMerge; mh++ {
				for mw := 0; mw < qwenSpatialMerge; mw++ {
					baseY := (bh*qwenSpatialMerge + mh) * qwenPatchSize
					baseX := (bw*qwenSpatialMerge + mw) * qwenPatchSize
					for c := 0; c < 3; c++ {
						for temporal := 0; temporal < qwenTemporalPatch; temporal++ {
							_ = temporal // 静态图复制同一图片形成 2 帧 temporal patch
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
	return out, nil
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
