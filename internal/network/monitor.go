package network

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

type Monitor struct {
	mu       sync.RWMutex
	client   *http.Client
	interval time.Duration
	endpoints []string
	status   []EndpointStatus
}

type EndpointStatus struct {
	URL       string
	Reachable bool
	Latency   time.Duration
	LastCheck time.Time
	Error     string
}

func NewMonitor(interval time.Duration) *Monitor {
	return &Monitor{
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 5 * time.Second,
				DisableKeepAlives:     false,
				MaxIdleConns:          2,
				IdleConnTimeout:       30 * time.Second,
			},
		},
		interval: interval,
	}
}

func (m *Monitor) Start(ctx context.Context, endpoints []string) {
	m.mu.Lock()
	m.endpoints = endpoints
	m.status = make([]EndpointStatus, len(endpoints))
	for i, ep := range endpoints {
		m.status[i] = EndpointStatus{URL: ep, Reachable: false}
	}
	m.mu.Unlock()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.checkAll(ctx)

	for {
		select {
		case <-ticker.C:
			m.checkAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Monitor) CheckOnce(ctx context.Context, endpoint string) EndpointStatus {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, "HEAD", endpoint, nil)
	if err != nil {
		return EndpointStatus{URL: endpoint, Reachable: false, Error: err.Error(), LastCheck: time.Now()}
	}

	resp, err := m.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return EndpointStatus{URL: endpoint, Reachable: false, Latency: latency, Error: err.Error(), LastCheck: time.Now()}
	}
	resp.Body.Close()

	return EndpointStatus{
		URL:       endpoint,
		Reachable: resp.StatusCode < 500,
		Latency:   latency,
		LastCheck: time.Now(),
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	m.mu.RLock()
	endpoints := m.endpoints
	m.mu.RUnlock()

	var wg sync.WaitGroup
	results := make([]EndpointStatus, len(endpoints))

	for i, ep := range endpoints {
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			results[idx] = m.CheckOnce(ctx, url)
		}(i, ep)
	}

	wg.Wait()

	m.mu.Lock()
	m.status = results
	m.mu.Unlock()
}

func (m *Monitor) Status() []EndpointStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := make([]EndpointStatus, len(m.status))
	copy(status, m.status)
	return status
}

func (m *Monitor) AllReachable() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, s := range m.status {
		if !s.Reachable {
			return false
		}
	}
	return len(m.status) > 0
}

func (m *Monitor) AggregateResult() types.NetworkCheckResult {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := types.NetworkCheckResult{LLMAPIReachable: true, DNSResolving: true}
	var totalLatency time.Duration
	checked := 0

	for _, s := range m.status {
		if !s.Reachable {
			result.LLMAPIReachable = false
			result.Error = fmt.Sprintf("endpoint %s unreachable: %s", s.URL, s.Error)
		}
		if s.Latency > 0 {
			totalLatency += s.Latency
			checked++
		}
	}

	if checked > 0 {
		result.Latency = totalLatency / time.Duration(checked)
	}

	result.DNSResolving = m.checkDNS()
	return result
}

func (m *Monitor) checkDNS() bool {
	_, err := net.LookupHost("google.com")
	if err != nil {
		_, err = net.LookupHost("baidu.com")
	}
	return err == nil
}
