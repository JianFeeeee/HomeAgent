//go:build onnxruntime

package nlp

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gitcode.com/JianFeeeee/HomeAgent/internal/config"
	ort "github.com/yalue/onnxruntime_go"
)

//go:embed models/*
var onnxModelFS embed.FS

type ONNXParser struct {
	rt      *ort.AdvancedSession
	vocab   map[string]int64
	posVocab map[string]int64
	Release func()
}

type ONNXConfig struct {
	ModelPath string // 留空使用内嵌模型
	DataDir   string // 模型解压/缓存目录
}

func NewONNXParser(cfg ONNXConfig) (*ONNXParser, error) {
	vocab, err := loadJSONMap[int64]("models/vocab.json", onnxModelFS)
	if err != nil {
		return nil, fmt.Errorf("load vocab: %w", err)
	}
	posVocab, err := loadJSONMap[int64]("models/pos_vocab.json", onnxModelFS)
	if err != nil {
		return nil, fmt.Errorf("load pos_vocab: %w", err)
	}

	modelPath := cfg.ModelPath
	if modelPath == "" {
		modelPath, err = extractEmbeddedModel(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("extract model: %w", err)
		}
	}

	ort.SetSharedLibraryPath(findONNXRuntime())
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("init onnx env: %w", err)
	}

	inputs := ort.NewInputDetails()
	inputs.Append("input_ids", []int64{1, 128})

	outputs := ort.NewOutputDetails()
	outputs.Append("pos_logits", []int64{1, 128, 18})
	outputs.Append("head_logits", []int64{1, 128, 128})
	outputs.Append("rel_logits", []int64{1, 128, 128, 18})

	session, err := ort.NewAdvancedSession(modelPath, inputs, outputs, nil)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	release := func() {
		session.Destroy()
		ort.DestroyEnvironment()
	}

	return &ONNXParser{
		rt:       session,
		vocab:    vocab,
		posVocab: posVocab,
		Release:  release,
	}, nil
}

func (p *ONNXParser) Parse(text string) (*ParseResult, error) {
	if text == "" {
		return &ParseResult{}, nil
	}

	inputIDs := tokenize(text, p.vocab, 128)
	inputIDs = padTo(inputIDs, 128)

	inputTensor, err := ort.NewTensor(ort.NewShape(1, 128), inputIDs)
	if err != nil {
		return nil, fmt.Errorf("create input tensor: %w", err)
	}
	defer inputTensor.Destroy()

	outputs, err := p.rt.Call(inputTensor)
	if err != nil {
		return nil, fmt.Errorf("onnx call: %w", err)
	}

	rawPOS := outputs[0].GetData().([]float32)
	rawHeads := outputs[1].GetData().([]float32)
	rawRels := outputs[2].GetData().([]float32)

	seqLen := actualLen(inputIDs)
	tokens := idsToTokens(inputIDs[:seqLen], p.vocab)
	pos := decodePOS(rawPOS, seqLen, p.posVocab)
	heads := decodeHeads(rawHeads, seqLen)
	rels := decodeRels(rawRels, seqLen)

	return &ParseResult{Tokens: tokens, POS: pos, Heads: heads, DepRels: rels}, nil
}

func loadJSONMap[T ~int64 | ~string](path string, fs embed.FS) (map[string]T, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Word map[string]T `json:"word"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		result := make(map[string]T)
		if err2 := json.Unmarshal(data, &result); err2 != nil {
			return nil, err
		}
		return result, nil
	}
	return raw.Word, nil
}

func extractEmbeddedModel(dataDir string) (string, error) {
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "homeagent-nlp")
	}
	os.MkdirAll(dataDir, 0755)
	dst := filepath.Join(dataDir, "dep_parser.onnx")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	data, err := onnxModelFS.ReadFile("models/dep_parser.onnx")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return "", err
	}
	return dst, nil
}

func findONNXRuntime() string {
	candidates := []string{
		"onnxruntime.dll",
		"libonnxruntime.so",
		"libonnxruntime.dylib",
		filepath.Join(os.Getenv("ONNXRUNTIME_DIR"), "libonnxruntime.so"),
		filepath.Join(os.Getenv("ONNXRUNTIME_DIR"), "onnxruntime.dll"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return "onnxruntime.dll"
}

func tokenize(text string, vocab map[string]int64, maxLen int) []int64 {
	ids := []int64{vocab["<bos>"]}
	runes := []rune(text)
	for i := 0; i < len(runes) && len(ids) < maxLen; i++ {
		if id, ok := vocab[string(runes[i])]; ok {
			ids = append(ids, id)
		} else {
			ids = append(ids, vocab["<unk>"])
		}
	}
	return ids
}

func padTo(ids []int64, length int) []int64 {
	for len(ids) < length {
		ids = append(ids, 0)
	}
	return ids
}

func actualLen(ids []int64) int {
	for i, id := range ids {
		if id == 0 {
			return i
		}
	}
	return len(ids)
}

func idsToTokens(ids []int64, vocab map[string]int64) []string {
	rev := make(map[int64]string)
	for k, v := range vocab {
		rev[v] = k
	}
	var tokens []string
	for _, id := range ids {
		if t, ok := rev[id]; ok {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

func decodePOS(raw []float32, seqLen int, posVocab map[string]int64) []string {
	rev := make(map[int64]string)
	for k, v := range posVocab {
		rev[v] = k
	}
	pos := make([]string, seqLen)
	for i := 0; i < seqLen; i++ {
		bestIdx := 0
		bestVal := float32(-1e9)
		for j := 0; j < 18; j++ {
			v := raw[i*18+j]
			if v > bestVal {
				bestVal = v
				bestIdx = j
			}
		}
		if tag, ok := rev[int64(bestIdx)]; ok {
			pos[i] = tag
		}
	}
	return pos
}

func decodeHeads(raw []float32, seqLen int) []int {
	heads := make([]int, seqLen)
	for i := 0; i < seqLen; i++ {
		bestIdx := 0
		bestVal := float32(-1e9)
		for j := 0; j < seqLen; j++ {
			v := raw[i*seqLen+j]
			if v > bestVal {
				bestVal = v
				bestIdx = j
			}
		}
		heads[i] = bestIdx
	}
	return heads
}

func decodeRels(raw []float32, seqLen int) []string {
	rels := make([]string, seqLen)
	for i := 0; i < seqLen; i++ {
		bestIdx := 0
		bestVal := float32(-1e9)
		for j := 0; j < 18; j++ {
			// average over head dimension for argmax
			var sum float32
			for k := 0; k < seqLen; k++ {
				sum += raw[i*seqLen*18+k*18+j]
			}
			avg := sum / float32(seqLen)
			if avg > bestVal {
				bestVal = avg
				bestIdx = j
			}
		}
		rels[i] = posIDToTag(bestIdx)
	}
	return rels
}

func posIDToTag(id int) string {
	tags := []string{"<bos>", "ADJ", "ADP", "ADV", "AUX", "CCONJ", "DET", "INTJ", "NOUN", "NUM", "PART", "PRON", "PROPN", "PUNCT", "SCONJ", "SYM", "VERB", "X"}
	if id >= 0 && id < len(tags) {
		return tags[id]
	}
	return "X"
}
