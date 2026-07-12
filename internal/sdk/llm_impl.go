package sdk

import agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"

type llmImpl struct{ mgr *agentAPI.ProviderManager }

func NewLLM(mgr *agentAPI.ProviderManager) LLMAPI { return &llmImpl{mgr: mgr} }

func (l *llmImpl) ListSources() []string {
	if l.mgr == nil { return nil }
	return l.mgr.List()
}

func (l *llmImpl) SetSource(name string) error {
	if l.mgr == nil { return nil }
	return l.mgr.SetDefault(name)
}

func (l *llmImpl) CurrentSource() string {
	if l.mgr == nil { return "" }
	p := l.mgr.Default()
	if p == nil { return "" }
	return p.Name()
}

var _ LLMAPI = (*llmImpl)(nil)
