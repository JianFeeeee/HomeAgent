//go:build !onnxruntime

package nlp

import "fmt"

type ONNXParser struct{}

type ONNXConfig struct {
	ModelPath string
	DataDir   string
}

func NewONNXParser(_ ONNXConfig) (*ONNXParser, error) {
	return nil, fmt.Errorf("ONNX parser requires build tag 'onnxruntime' (go build -tags onnxruntime)")
}

func (p *ONNXParser) Parse(_ string) (*ParseResult, error) {
	return nil, fmt.Errorf("ONNX parser not available: rebuild with -tags onnxruntime")
}
