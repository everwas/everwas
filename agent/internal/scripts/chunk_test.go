package scripts

import (
	"bytes"
	"encoding/json"
	"testing"
)

// collect returns a sink that records chunks in memory.
func collect(t *testing.T, maxChunk, capBytes int) (*chunkSink, *[]Chunk) {
	t.Helper()
	var got []Chunk
	s := newChunkSink("job-1", "", func(c Chunk) error {
		// copy: the runner reuses its read buffer
		c.Data = append([]byte(nil), c.Data...)
		got = append(got, c)
		return nil
	})
	s.maxChunk, s.capBytes = maxChunk, capBytes
	return s, &got
}

func TestChunkSinkFraming(t *testing.T) {
	tests := []struct {
		name       string
		maxChunk   int
		writes     []int // byte counts per write
		wantChunks []int // expected chunk sizes in order
	}{
		{"empty write emits nothing", 4, []int{0}, nil},
		{"single byte", 4, []int{1}, []int{1}},
		{"exactly one chunk", 4, []int{4}, []int{4}},
		{"one byte over", 4, []int{5}, []int{4, 1}},
		{"two full chunks", 4, []int{8}, []int{4, 4}},
		{"writes are not merged", 4, []int{3, 3}, []int{3, 3}},
		{"large write splits evenly", 4, []int{12}, []int{4, 4, 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, got := collect(t, tt.maxChunk, 1<<20)
			for _, n := range tt.writes {
				if err := s.write(StreamStdout, bytes.Repeat([]byte("x"), n)); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if len(*got) != len(tt.wantChunks) {
				t.Fatalf("got %d chunks, want %d", len(*got), len(tt.wantChunks))
			}
			for i, want := range tt.wantChunks {
				if len((*got)[i].Data) != want {
					t.Errorf("chunk %d: %d bytes, want %d", i, len((*got)[i].Data), want)
				}
				if (*got)[i].Seq != i+1 {
					t.Errorf("chunk %d: seq %d, want %d", i, (*got)[i].Seq, i+1)
				}
			}
		})
	}
}

func TestChunkSinkSeqSpansStreams(t *testing.T) {
	s, got := collect(t, 8, 1<<20)
	mustWrite(t, s, StreamStdout, 4)
	mustWrite(t, s, StreamStderr, 4)
	mustWrite(t, s, StreamStdout, 4)

	want := []struct {
		stream string
		seq    int
	}{{StreamStdout, 1}, {StreamStderr, 2}, {StreamStdout, 3}}
	for i, w := range want {
		if (*got)[i].Stream != w.stream || (*got)[i].Seq != w.seq {
			t.Errorf("chunk %d: %s/%d, want %s/%d",
				i, (*got)[i].Stream, (*got)[i].Seq, w.stream, w.seq)
		}
	}
}

func TestChunkSinkCap(t *testing.T) {
	tests := []struct {
		name          string
		capBytes      int
		writes        []int
		wantTotal     int
		wantTruncated bool
	}{
		{"under cap", 10, []int{4, 4}, 8, false},
		{"exactly at cap is not truncation", 10, []int{5, 5}, 10, false},
		{"one byte over", 10, []int{5, 6}, 10, true},
		{"single oversized write", 10, []int{25}, 10, true},
		{"writes after cap are dropped", 10, []int{11, 5, 5}, 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, got := collect(t, 4, tt.capBytes)
			for _, n := range tt.writes {
				mustWrite(t, s, StreamStdout, n)
			}
			total := 0
			for _, c := range *got {
				total += len(c.Data)
			}
			if total != tt.wantTotal {
				t.Errorf("emitted %d bytes, want %d", total, tt.wantTotal)
			}
			if s.isTruncated() != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", s.isTruncated(), tt.wantTruncated)
			}
		})
	}
}

func TestChunkSinkEOFAlwaysEmitted(t *testing.T) {
	s, got := collect(t, 4, 4)
	mustWrite(t, s, StreamStdout, 100) // blows the cap
	if err := s.eof(StreamStdout); err != nil {
		t.Fatalf("eof: %v", err)
	}
	last := (*got)[len(*got)-1]
	if !last.EOF || len(last.Data) != 0 {
		t.Errorf("last chunk = %+v, want empty eof marker", last)
	}
}

// TestChunkJSONRoundTrip pins the wire encoding: Data must travel as
// standard base64 and come back byte-identical, including NUL and high
// bytes that a string field would mangle.
func TestChunkJSONRoundTrip(t *testing.T) {
	payload := []byte{0x00, 0x01, 0xff, 0xfe, '\n', 'h', 'i', 0x1b, '[', '0', 'm'}
	raw, err := json.Marshal(Chunk{
		JobID: "job-1", Stream: StreamStderr, Seq: 7, Data: payload,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := asMap["data"].(string); !ok {
		t.Fatalf("data field is %T, want base64 string", asMap["data"])
	}
	if asMap["data"].(string) != "AAH//gpoaRtbMG0=" {
		t.Errorf("base64 = %q, want standard-alphabet encoding", asMap["data"])
	}
	var back Chunk
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(back.Data, payload) {
		t.Errorf("round trip = %v, want %v", back.Data, payload)
	}
}

func mustWrite(t *testing.T, s *chunkSink, stream string, n int) {
	t.Helper()
	if err := s.write(stream, bytes.Repeat([]byte("x"), n)); err != nil {
		t.Fatalf("write: %v", err)
	}
}
