package scripts

import "sync"

const (
	// MaxChunkBytes is the largest payload we put in one output message.
	// 256 KiB keeps us well under the NATS 1 MiB default max_payload once
	// base64 (4/3) and the envelope are accounted for.
	MaxChunkBytes = 256 * 1024

	// MaxJobOutputBytes caps how much output we capture per job. Past this
	// we stop capturing and flag the result truncated; the script keeps
	// running.
	MaxJobOutputBytes = 8 * 1024 * 1024
)

// Stream names carried in a chunk.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// Chunk is one framed piece of job output. Data is a []byte so encoding/json
// emits it as standard base64, which is what the wire contract specifies.
type Chunk struct {
	JobID  string `json:"job_id"`
	Stream string `json:"stream"`
	Seq    int    `json:"seq"`
	Data   []byte `json:"data"`
	EOF    bool   `json:"eof"`
}

// chunkSink frames process output into Chunks. seq is a single sequence
// shared by both streams, so the server can reconstruct the interleaving
// exactly as the agent saw it.
type chunkSink struct {
	jobID    string
	maxChunk int
	capBytes int
	emit     func(Chunk) error

	mu        sync.Mutex
	seq       int
	written   int
	truncated bool
	stopped   bool
}

func newChunkSink(jobID string, emit func(Chunk) error) *chunkSink {
	return &chunkSink{
		jobID:    jobID,
		maxChunk: MaxChunkBytes,
		capBytes: MaxJobOutputBytes,
		emit:     emit,
	}
}

// write frames p into one or more chunks, respecting the per-job cap. Once
// the cap is hit the sink stops emitting data for the rest of the job.
func (s *chunkSink) write(stream string, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) == 0 || s.truncated || s.stopped {
		return nil
	}
	// Only flag truncation when bytes are actually dropped: a write that
	// exactly fills the cap loses nothing.
	if remaining := s.capBytes - s.written; len(p) > remaining {
		p = p[:remaining]
		s.truncated = true
	}
	for len(p) > 0 {
		n := min(len(p), s.maxChunk)
		if err := s.send(stream, p[:n], false); err != nil {
			return err
		}
		s.written += n
		p = p[n:]
	}
	return nil
}

// eof emits the terminal zero-length chunk for a stream. It is sent even
// when output was truncated so the server always sees a clean end marker.
func (s *chunkSink) eof(stream string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send(stream, nil, true)
}

func (s *chunkSink) send(stream string, data []byte, eof bool) error {
	s.seq++
	return s.emit(Chunk{
		JobID:  s.jobID,
		Stream: stream,
		Seq:    s.seq,
		Data:   data,
		EOF:    eof,
	})
}

// stop drops any further data. It is called when a job is reported while a
// reader is still blocked on a pipe some descendant holds open: that reader
// must not publish output after the stream's EOF marker. eof still works,
// so the streams are always closed cleanly.
func (s *chunkSink) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
}

// isTruncated reports whether the per-job cap was reached.
func (s *chunkSink) isTruncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}
