package network

import "testing"

func TestAggregateResultNoEndpoints(t *testing.T) {
	m := NewMonitor(0)
	res := m.AggregateResult()
	if res.EndpointsConfigured {
		t.Error("no endpoints configured should be false")
	}
	if res.LLMAPIReachable {
		t.Error("LLMAPIReachable must not be true when no endpoints configured")
	}
	if res.Error == "" {
		t.Error("should report no-endpoints error")
	}
}

func TestAggregateResultWithEndpointsUnreachable(t *testing.T) {
	m := NewMonitor(0)
	m.mu.Lock()
	m.status = []EndpointStatus{{URL: "http://127.0.0.1:1/", Reachable: false, Error: "dial refused"}}
	m.mu.Unlock()

	res := m.AggregateResult()
	if !res.EndpointsConfigured {
		t.Error("endpoints configured should be true")
	}
	if res.LLMAPIReachable {
		t.Error("unreachable endpoint should make LLMAPIReachable false")
	}
}

func TestAllReachableEmpty(t *testing.T) {
	m := NewMonitor(0)
	if m.AllReachable() {
		t.Error("AllReachable must be false with no endpoints (not vacuously true)")
	}
}
