package main

import (
	"os"
	"strings"
	"sync"
)

type History struct {
	path  string
	lines []string
	max   int
	mu    sync.Mutex
}

func newHistory(path string, max int) *History {
	return &History{path: path, max: max}
}

func (h *History) load() {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	h.lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(h.lines) > h.max {
		h.lines = h.lines[len(h.lines)-h.max:]
	}
}

func (h *History) save() {
	h.mu.Lock()
	defer h.mu.Unlock()
	data := strings.Join(h.lines, "\n") + "\n"
	os.WriteFile(h.path, []byte(data), 0644)
}

func (h *History) add(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) > 0 && h.lines[len(h.lines)-1] == line {
		return
	}
	h.lines = append(h.lines, line)
	if len(h.lines) > h.max {
		h.lines = h.lines[len(h.lines)-h.max:]
	}
}

func (h *History) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := make([]string, len(h.lines))
	copy(r, h.lines)
	return r
}
