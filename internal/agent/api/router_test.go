package api

import (
	"context"
	"testing"
)

// stubRoutableProvider implements both Provider and RoutableProvider.
type stubRoutableProvider struct {
	name    string
	model   string
	prio    int
}

func (s *stubRoutableProvider) Name() string          { return s.name }
func (s *stubRoutableProvider) Model() string         { return s.model }
func (s *stubRoutableProvider) Priority() int         { return s.prio }
func (s *stubRoutableProvider) MaxContextTokens() int { return 4096 }
func (s *stubRoutableProvider) Chat(context.Context, *CompletionRequest) (*CompletionResponse, error) {
	return &CompletionResponse{Content: s.name}, nil
}
func (s *stubRoutableProvider) ChatStream(context.Context, *CompletionRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Done: true}
	return ch, nil
}

func TestProviderManagerPriorityOrder(t *testing.T) {
	m := NewProviderManager()
	m.Register("low", &stubRoutableProvider{name: "low", prio: 10})
	m.Register("high", &stubRoutableProvider{name: "high", prio: 90})
	m.Register("mid", &stubRoutableProvider{name: "mid", prio: 50})

	got := m.OrderedProviders()
	wantOrder := []string{"high", "mid", "low"}
	for i, p := range got {
		if p.Name() != wantOrder[i] {
			t.Fatalf("order[%d] = %s, want %s (full=%v)", i, p.Name(), wantOrder[i], names(got))
		}
	}
}

func TestProviderManagerResolveForModel(t *testing.T) {
	m := NewProviderManager()
	m.Register("a", &stubRoutableProvider{name: "a", model: "gpt-5"})
	m.Register("b", &stubRoutableProvider{name: "b", model: "deepseek"})

	got := m.ResolveForModel("deepseek")
	if len(got) == 0 || got[0].Name() != "b" {
		t.Fatalf("resolve deepseek: got %v", names(got))
	}

	// 未知模型回落 AUTO 链（仍按优先级）
	got2 := m.ResolveForModel("unknown")
	if len(got2) == 0 {
		t.Fatal("unknown model should fall back to auto chain")
	}
}

func names(ps []Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}