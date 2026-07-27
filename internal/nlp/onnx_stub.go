//go:build !onnxruntime

package nlp

import "fmt"

// ONNXParserStub 占位 — 编译时未启用 onnxruntime
type ONNXParser struct{}

type ONNXConfig struct {
	ModelPath  string
	VocabPath  string
	POSVocPath string
}

func NewONNXParser(cfg ONNXConfig) (*ONNXParser, error) {
	return nil, fmt.Errorf("onnxparser: build with -tags onnxruntime to enable")
}

func (p *ONNXParser) Close() {}

func (p *ONNXParser) Parse(text string) (*ParseResult, error) {
	return nil, fmt.Errorf("onnxparser: not available (build with -tags onnxruntime)")
}

func (p *ONNXParser) EnsureModel(dataDir string) error {
	return fmt.Errorf("onnxparser: not available")
}
