package main

import (
	"strings"
	"sync"
	"time"
)

// logBuffer keeps the last lines the importer printed, for the dashboard.
// It is safe for concurrent use.
type logBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func newLogBuffer(max int) *logBuffer {
	return &logBuffer{max: max}
}

func (b *logBuffer) Write(p []byte) (int, error) {
	stamp := time.Now().UTC().Format("15:04:05")
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		b.lines = append(b.lines, stamp+"  "+line)
	}
	if over := len(b.lines) - b.max; over > 0 {
		b.lines = append([]string(nil), b.lines[over:]...)
	}
	return len(p), nil
}

// Lines returns a copy of the buffered lines, oldest first.
func (b *logBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

func (b *logBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = nil
}
