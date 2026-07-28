//go:build onnxruntime

package nlp

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	ort "github.com/yalue/onnxruntime_go"
)

//go:embed models/*
var onnxModelFS embed.FS

const maxSeqLen = 128

type ONNXParser struct {
	rt       *ort.DynamicAdvancedSession
	vocab    map[string]int64
	posVocab map[string]int64
	close    sync.Once
}

type ONNXConfig struct {
	ModelPath string
	DataDir   string
}

func NewONNXParser(cfg ONNXConfig) (*ONNXParser, error) {
	vocab, err := loadWordMap("models/vocab.json")
	if err != nil {
		return nil, fmt.Errorf("load vocab: %w", err)
	}
	posVocab, err := loadWordMap("models/pos_vocab.json")
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

	ort.SetSharedLibraryPath(libPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("init onnx env: %w", err)
	}

	inputNames := []string{"input_ids"}
	outputNames := []string{"pos_logits", "head_logits", "rel_logits"}

	session, err := ort.NewDynamicAdvancedSession(modelPath, inputNames, outputNames, nil)
	if err != nil {
		ort.DestroyEnvironment()
		return nil, fmt.Errorf("create session: %w", err)
	}

	return &ONNXParser{
		rt:       session,
		vocab:    vocab,
		posVocab: posVocab,
	}, nil
}

func (p *ONNXParser) Close() error {
	p.close.Do(func() {
		p.rt.Destroy()
		ort.DestroyEnvironment()
	})
	return nil
}

func (p *ONNXParser) Parse(text string) (*ParseResult, error) {
	if text == "" {
		return &ParseResult{}, nil
	}

	x := memory.GetJieba()
	if x == nil {
		return nil, fmt.Errorf("jieba unavailable")
	}
	words := x.Cut(text, true)
	if len(words) == 0 {
		return &ParseResult{}, nil
	}

	inIDs := p.wordsToIDs(words, maxSeqLen)
	n := len(inIDs) - 1 // exclude <bos>
	if n <= 0 {
		return &ParseResult{}, nil
	}
	if n > len(words) {
		n = len(words)
	}

	padded := padTo(inIDs, maxSeqLen)

	inTensor, err := ort.NewTensor(ort.NewShape(1, maxSeqLen), padded)
	if err != nil {
		return nil, fmt.Errorf("create input tensor: %w", err)
	}
	defer inTensor.Destroy()

	outputs := make([]ort.Value, 3)
	if err := p.rt.Run([]ort.Value{inTensor}, outputs); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}

	posOut, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("pos output not Tensor[float32]")
	}
	headOut, ok := outputs[1].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("head output not Tensor[float32]")
	}
	relOut, ok := outputs[2].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("rel output not Tensor[float32]")
	}
	defer posOut.Destroy()
	defer headOut.Destroy()
	defer relOut.Destroy()

	posShape := posOut.GetShape()   // [1, seq, posDim]
	headShape := headOut.GetShape() // [1, seq, seq]
	relShape := relOut.GetShape()   // [1, seq, seq, relDim]

	if len(posShape) < 3 || len(headShape) < 3 || len(relShape) < 4 {
		return nil, fmt.Errorf("unexpected output ranks: pos=%d head=%d rel=%d",
			len(posShape), len(headShape), len(relShape))
	}

	seqDim := int(headShape[1])
	posDim := int(posShape[2])
	relDim := int(relShape[3])

	if n > seqDim {
		n = seqDim
	}

	rawPOS := posOut.GetData()
	rawHeads := headOut.GetData()
	rawRels := relOut.GetData()

	pos := decodePOS(rawPOS, n, posDim, p.posVocab)
	heads := decodeHeads(rawHeads, n, seqDim)
	rels := decodeRels(rawRels, n, seqDim, relDim, heads)

	return &ParseResult{
		Tokens:  words[:n],
		POS:     pos,
		Heads:   heads,
		DepRels: rels,
	}, nil
}

func (p *ONNXParser) wordsToIDs(words []string, maxLen int) []int64 {
	ids := make([]int64, 0, maxLen)
	if bos, ok := p.vocab["<bos>"]; ok {
		ids = append(ids, bos)
	}
	for _, w := range words {
		if len(ids) >= maxLen {
			break
		}
		if id, ok := p.vocab[w]; ok {
			ids = append(ids, id)
		} else if unk, ok := p.vocab["<unk>"]; ok {
			ids = append(ids, unk)
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

func decodePOS(raw []float32, n, posDim int, posVocab map[string]int64) []string {
	rev := make(map[int64]string)
	for k, v := range posVocab {
		rev[v] = k
	}
	pos := make([]string, n)
	for i := 0; i < n; i++ {
		bestIdx := 0
		bestVal := float32(-1e9)
		for j := 0; j < posDim; j++ {
			if v := raw[i*posDim+j]; v > bestVal {
				bestVal = v
				bestIdx = j
			}
		}
		if tag, ok := rev[int64(bestIdx)]; ok {
			pos[i] = tag
		} else {
			pos[i] = "X"
		}
	}
	return pos
}

func decodeHeads(raw []float32, n, seqDim int) []int {
	heads := make([]int, n)
	for i := 0; i < n; i++ {
		bestIdx := 0
		bestVal := float32(-1e9)
		for j := 0; j < seqDim; j++ {
			if v := raw[i*seqDim+j]; v > bestVal {
				bestVal = v
				bestIdx = j
			}
		}
		heads[i] = bestIdx
	}
	return heads
}

func decodeRels(raw []float32, n, seqDim, relDim int, heads []int) []string {
	rels := make([]string, n)
	stride := seqDim * relDim
	for i := 0; i < n; i++ {
		h := heads[i]
		if h < 0 || h >= seqDim {
			rels[i] = "dep"
			continue
		}
		bestIdx := 0
		bestVal := float32(-1e9)
		for r := 0; r < relDim; r++ {
			if v := raw[i*stride+h*relDim+r]; v > bestVal {
				bestVal = v
				bestIdx = r
			}
		}
		rels[i] = depRelLabel(bestIdx)
	}
	return rels
}

func loadWordMap(path string) (map[string]int64, error) {
	data, err := onnxModelFS.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Word map[string]int64 `json:"word"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		var flat map[string]int64
		if err2 := json.Unmarshal(data, &flat); err2 != nil {
			return nil, err
		}
		return flat, nil
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

func libPath() string {
	for _, env := range []string{"ONNXRUNTIME_DIR", "ONNX_ML_DIR"} {
		if d := os.Getenv(env); d != "" {
			for _, name := range []string{"libonnxruntime.so", "libonnxruntime.dylib", "onnxruntime.dll"} {
				if candidate := filepath.Join(d, name); fileExists(candidate) {
					return candidate
				}
			}
		}
	}
	for _, name := range []string{"libonnxruntime.so", "libonnxruntime.dylib", "onnxruntime.dll"} {
		if fileExists(name) {
			abs, _ := filepath.Abs(name)
			return abs
		}
	}
	return "onnxruntime.dll"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func depRelLabel(id int) string {
	labels := []string{"root", "nsubj", "obj", "iobj", "obl", "vocative", "expl", "csubj", "ccomp", "xcomp",
		"advcl", "advmod", "amod", "appos", "nmod", "acl", "det", "clf", "case", "mark",
		"nummod", "discourse", "aux", "cop", "cc", "conj", "fixed", "flat", "list", "parataxis",
		"orphan", "goeswith", "reparandum", "punct", "dep"}
	if id >= 0 && id < len(labels) {
		return labels[id]
	}
	return "dep"
}
