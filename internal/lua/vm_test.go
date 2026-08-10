package lua

import (
	"strings"
	"sync"
	"testing"
)

func TestVMLoadAndTransform(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()

	out, err := vm.CallTransformRequest("openai", `{"model":"x"}`)
	if err != nil {
		t.Fatalf("transform_request: %v", err)
	}
	if !strings.Contains(out, `"model":"x"`) {
		t.Fatalf("unexpected output: %s", out)
	}

	ep := vm.GetAdapterEndpoint("openai")
	if ep == "" {
		t.Fatal("openai endpoint empty")
	}
	hdrs := vm.GetAdapterHeaders("openai")
	if hdrs == nil {
		t.Fatal("openai headers nil")
	}
}

func TestVMLoadCustomAdapter(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()

	code := `
local adapter = {}
adapter.name = "testa"
adapter.version = "1.0"
adapter.endpoint = "/x"
adapter.headers = { ["X-A"] = "1" }
function adapter.transform_request(raw) return "REQ" end
function adapter.build_headers(meta) return { ["X-Key"] = "k" } end
function adapter.transform_response(raw) return "RESP" end
function adapter.transform_stream_chunk(raw) return "CHUNK" end
return adapter
`
	if err := vm.LoadAdapterSource("testa", code); err != nil {
		t.Fatalf("load adapter: %v", err)
	}

	if out, err := vm.CallTransformRequest("testa", "x"); err != nil || out != "REQ" {
		t.Fatalf("req=%q err=%v", out, err)
	}
	if out, err := vm.CallTransformResponse("testa", "x"); err != nil || out != "RESP" {
		t.Fatalf("resp=%q err=%v", out, err)
	}
	if out, err := vm.CallTransformStreamChunk("testa", "x"); err != nil || out != "CHUNK" {
		t.Fatalf("chunk=%q err=%v", out, err)
	}

	meta := map[string]interface{}{"url": "http://x", "api_key": "k1"}
	hdrs, err := vm.BuildHeaders("testa", meta)
	if err != nil {
		t.Fatalf("build_headers: %v", err)
	}
	if hdrs["X-Key"] != "k" {
		t.Fatalf("missing dynamic header: %v", hdrs)
	}

	if ep := vm.GetAdapterEndpoint("testa"); ep != "/x" {
		t.Fatalf("endpoint=%q", ep)
	}
	if st := vm.GetAdapterHeaders("testa"); st["X-A"] != "1" {
		t.Fatalf("static headers=%v", st)
	}
}

func TestVMLoadStaticHeaderFallback(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()

	code := `
local adapter = {}
adapter.name = "statics"
adapter.headers = { ["X-S"] = "s" }
function adapter.transform_request(raw) return raw end
return adapter
`
	if err := vm.LoadAdapterSource("statics", code); err != nil {
		t.Fatalf("load: %v", err)
	}
	hdrs, err := vm.BuildHeaders("statics", nil)
	if err != nil {
		t.Fatalf("build_headers: %v", err)
	}
	if hdrs["X-S"] != "s" {
		t.Fatalf("expected static fallback, got %v", hdrs)
	}
}

func TestVMConcurrentCalls(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, err := vm.CallTransformRequest("openai", `{"model":"x"}`)
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent call: %v", err)
	}
}
func TestOllamaMultimodal(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()

	raw := `{"model":"llama3","messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	out, err := vm.CallTransformRequest("ollama", raw)
	if err != nil {
		t.Fatalf("transform_request: %v", err)
	}
	if !strings.Contains(out, `"images":["AAAA"]`) {
		t.Fatalf("expected base64 images array, got: %s", out)
	}
	if !strings.Contains(out, `"content":"look"`) {
		t.Fatalf("text not preserved: %s", out)
	}
}

func TestAnthropicMultimodal(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()

	raw := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAEC"}}]}]}`
	out, err := vm.CallTransformRequest("anthropic", raw)
	if err != nil {
		t.Fatalf("transform_request: %v", err)
	}
	if !strings.Contains(out, `"type":"image"`) || !strings.Contains(out, `"media_type":"image/png"`) || !strings.Contains(out, `"data":"AAEC"`) {
		t.Fatalf("expected anthropic image source block, got: %s", out)
	}
	if !strings.Contains(out, `"type":"text"`) {
		t.Fatalf("expected text block preserved: %s", out)
	}
}

func TestOpenAIPassthroughKeepsMultimodal(t *testing.T) {
	vm := NewVM(t.TempDir())
	if err := vm.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	defer vm.Stop()
	raw := `{"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`
	out, err := vm.CallTransformRequest("openai", raw)
	if err != nil {
		t.Fatalf("transform_request: %v", err)
	}
	if !strings.Contains(out, `"image_url"`) || !strings.Contains(out, "QUJD") {
		t.Fatalf("openai passthrough dropped multimodal: %s", out)
	}
}
