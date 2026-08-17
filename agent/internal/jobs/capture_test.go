package jobs

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/rsp2k/openrmm/agent/internal/scripts"
)

// captured collects what a Module published, without a NATS server.
type captured struct {
	mu      sync.Mutex
	results []scripts.Result
	chunks  []scripts.Chunk
}

func (c *captured) lastResult(t *testing.T) scripts.Result {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) == 0 {
		t.Fatal("no terminal result was published: the server is left waiting forever")
	}
	return c.results[len(c.results)-1]
}

func (c *captured) anyChunkHasEntry(entryID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.chunks {
		if ch.EntryID == entryID {
			return true
		}
	}
	return false
}

func (c *captured) chunkEntries() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.chunks))
	for _, ch := range c.chunks {
		out = append(out, ch.EntryID)
	}
	return out
}

func moduleWithCapture() (*Module, *captured) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cap := &captured{}
	runner := scripts.NewRunner(nil, "agent-1", "", nil, log)
	runner.OnResult = func(_ string, res scripts.Result) {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		cap.results = append(cap.results, res)
	}
	runner.OnChunk = func(ch scripts.Chunk) error {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		cap.chunks = append(cap.chunks, ch)
		return nil
	}
	return &Module{Scripts: runner, Log: log, AgentID: "agent-1"}, cap
}
