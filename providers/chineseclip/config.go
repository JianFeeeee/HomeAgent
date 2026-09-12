package chineseclip

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// embedConfig 是产物目录里 embed_config.json 的映射：provider 的全部模型假设
// 都来自这个文件，不在代码里散落魔数。
type embedConfig struct {
	Arch        string    `json:"arch"`
	Dimension   int       `json:"dim"`
	TextONNX    string    `json:"text_onnx"`
	VisionONNX  string    `json:"vision_onnx"`
	MaxLength   int       `json:"max_length"`
	ImageSize   int       `json:"image_size"`
	ImageMean   []float64 `json:"image_mean"`
	ImageStd    []float64 `json:"image_std"`
	Normalize   bool      `json:"normalize_vector"`
	Modalities  []string  `json:"modalities"`
	Unsupported []string  `json:"unsupported_modalities"`
}

// loadConfig 读取并校验产物配置。任何不匹配都必须**明确报错**：
// 静默沿用默认值会在换错模型时产出「看起来正常、语义错误」的向量，
// 那类错误会污染整个图记忆且难以追查。
func loadConfig(dir string) (embedConfig, error) {
	path := filepath.Join(dir, "embed_config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return embedConfig{}, fmt.Errorf("chineseclip: 读取 %s: %w", path, err)
	}
	var cfg embedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return embedConfig{}, fmt.Errorf("chineseclip: 解析 %s: %w", path, err)
	}
	if cfg.Dimension != 512 {
		return embedConfig{}, fmt.Errorf("chineseclip: 维度不匹配 dim=%d（期望 512）", cfg.Dimension)
	}
	if cfg.MaxLength <= 0 || cfg.MaxLength > 512 {
		return embedConfig{}, fmt.Errorf("chineseclip: max_length 非法: %d", cfg.MaxLength)
	}
	if cfg.ImageSize != 224 {
		return embedConfig{}, fmt.Errorf("chineseclip: image_size 不匹配 %d（期望 224）", cfg.ImageSize)
	}
	if len(cfg.ImageMean) != 3 || len(cfg.ImageStd) != 3 {
		return embedConfig{}, fmt.Errorf("chineseclip: image_mean/std 必须各 3 个分量，得到 %d/%d",
			len(cfg.ImageMean), len(cfg.ImageStd))
	}
	for i := range cfg.ImageStd {
		if cfg.ImageStd[i] == 0 {
			return embedConfig{}, fmt.Errorf("chineseclip: image_std[%d] 为 0", i)
		}
	}
	if cfg.TextONNX == "" || cfg.VisionONNX == "" {
		return embedConfig{}, fmt.Errorf("chineseclip: 未声明 onnx 文件名（text=%q vision=%q）",
			cfg.TextONNX, cfg.VisionONNX)
	}
	for _, name := range []string{cfg.TextONNX, cfg.VisionONNX, "vocab.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return embedConfig{}, fmt.Errorf("chineseclip: 产物缺少 %s: %w", name, err)
		}
	}
	return cfg, nil
}
