package shell

import (
	"context"
	"testing"
	"time"
)

// TestFlowControllerThresholds walks the accounting the way a session does,
// without needing a PTY: publish bytes, receive acks, observe pause state.
func TestFlowControllerThresholds(t *testing.T) {
	type step struct {
		sent       int   // bytes published (0 for an ack step)
		ack        int64 // bytes acknowledged
		wantPaused bool
		wantUnaked int64
	}
	tests := []struct {
		name  string
		pause int64
		below int64
		steps []step
	}{
		{
			name: "stays running below the threshold", pause: 100, below: 50,
			steps: []step{
				{sent: 40, wantPaused: false, wantUnaked: 40},
				{sent: 40, wantPaused: false, wantUnaked: 80},
				{sent: 20, wantPaused: false, wantUnaked: 100}, // exactly at, not over
			},
		},
		{
			name: "pauses one byte over the threshold", pause: 100, below: 50,
			steps: []step{
				{sent: 101, wantPaused: true, wantUnaked: 101},
			},
		},
		{
			name: "an ack that only partly drains keeps it paused", pause: 100, below: 50,
			steps: []step{
				{sent: 200, wantPaused: true, wantUnaked: 200},
				{ack: 100, wantPaused: true, wantUnaked: 100},
				{ack: 49, wantPaused: true, wantUnaked: 51},
				{ack: 1, wantPaused: false, wantUnaked: 50}, // at resume threshold
			},
		},
		{
			name: "resumes and can pause again", pause: 100, below: 50,
			steps: []step{
				{sent: 150, wantPaused: true, wantUnaked: 150},
				{ack: 150, wantPaused: false, wantUnaked: 0},
				{sent: 101, wantPaused: true, wantUnaked: 101},
			},
		},
		{
			name: "over-acking never goes negative", pause: 100, below: 50,
			steps: []step{
				{sent: 10, wantPaused: false, wantUnaked: 10},
				{ack: 999, wantPaused: false, wantUnaked: 0},
			},
		},
		{
			name: "a zero ack is ignored", pause: 100, below: 50,
			steps: []step{
				{sent: 150, wantPaused: true, wantUnaked: 150},
				{ack: 0, wantPaused: true, wantUnaked: 150},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFlowControllerWith(tt.pause, tt.below)
			for i, st := range tt.steps {
				if st.sent > 0 {
					f.sent(st.sent)
				}
				if st.ack != 0 {
					f.ack(st.ack)
				}
				if got := f.isPaused(); got != st.wantPaused {
					t.Errorf("step %d: paused = %v, want %v", i, got, st.wantPaused)
				}
				if got := f.unackedBytes(); got != st.wantUnaked {
					t.Errorf("step %d: unacked = %d, want %d", i, got, st.wantUnaked)
				}
			}
		})
	}
}

func TestFlowControllerDefaults(t *testing.T) {
	f := newFlowController()
	f.sent(PauseAbove)
	if f.isPaused() {
		t.Errorf("paused at exactly %d un-acked bytes; the contract says pause above", PauseAbove)
	}
	f.sent(1)
	if !f.isPaused() {
		t.Error("not paused past 512 KiB un-acked")
	}
	// Hysteresis: draining to just above ResumeBelow must not resume.
	f.ack(int64(PauseAbove + 1 - ResumeBelow - 1))
	if !f.isPaused() {
		t.Error("resumed before dropping to the resume threshold")
	}
	f.ack(1)
	if f.isPaused() {
		t.Error("still paused at the resume threshold")
	}
}

func TestFlowControllerWaitBlocksUntilAck(t *testing.T) {
	f := newFlowControllerWith(10, 5)
	if err := f.wait(context.Background()); err != nil {
		t.Fatalf("wait while running: %v", err)
	}
	f.sent(20)

	resumed := make(chan error, 1)
	go func() { resumed <- f.wait(context.Background()) }()
	select {
	case <-resumed:
		t.Fatal("wait returned while paused")
	case <-time.After(20 * time.Millisecond):
	}

	f.ack(20)
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return after the ack")
	}
}

func TestFlowControllerWaitHonoursContext(t *testing.T) {
	f := newFlowControllerWith(10, 5)
	f.sent(20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.wait(ctx); err == nil {
		t.Error("wait should return the context error on teardown")
	}
}

func TestFlowControllerReleaseUnblocks(t *testing.T) {
	f := newFlowControllerWith(10, 5)
	f.sent(20)
	done := make(chan struct{})
	go func() { _ = f.wait(context.Background()); close(done) }()
	f.release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("release did not unblock the pump")
	}
	if f.isPaused() {
		t.Error("release left the controller paused")
	}
}
