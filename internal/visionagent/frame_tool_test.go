package visionagent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type fakeFrameExtractor struct {
	calls []time.Duration
}

type cancellationFrameExtractor struct{}

func (cancellationFrameExtractor) ExtractFrameAt(ctx context.Context, _, _ string, _ time.Duration) error {
	return ctx.Err()
}

func (f *fakeFrameExtractor) ExtractFrameAt(_ context.Context, _, output string, timestamp time.Duration) error {
	f.calls = append(f.calls, timestamp)
	return os.WriteFile(output, []byte("jpeg"), 0o600)
}

func TestSessionReusesFramesAndEnforcesAuthorizedRange(t *testing.T) {
	extractor := &fakeFrameExtractor{}
	s, err := newSession(context.Background(), Range{StartMS: 0, EndMS: 30_000}, "video", func() {}, extractor)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitialSample(); err != nil {
		t.Fatal(err)
	}
	if len(s.frames) != 6 || len(extractor.calls) != 6 {
		t.Fatalf("initial frames/calls = %d/%d, want 6/6", len(s.frames), len(extractor.calls))
	}
	// 第二次包含旧时间点时只提取新增帧，预算也只计算唯一帧。
	if err := s.ExecuteTool(InspectVideoRangeInput{StartMS: 0, EndMS: 2_000, IntervalMS: 500}); err != nil {
		t.Fatal(err)
	}
	if len(s.frames) != 9 || len(extractor.calls) != 9 {
		t.Fatalf("deduplicated frames/calls = %d/%d, want 9/9", len(s.frames), len(extractor.calls))
	}
	if err := s.ExecuteTool(InspectVideoRangeInput{StartMS: 29_000, EndMS: 31_000, IntervalMS: 500}); !errorsIs(err, ErrRangeOutOfBounds) {
		t.Fatalf("out-of-range error = %v", err)
	}
}

func TestInvalidToolCallConsumesBudget(t *testing.T) {
	s, err := newSession(context.Background(), Range{StartMS: 0, EndMS: 30_000}, "video", func() {}, &fakeFrameExtractor{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for attempt := 0; attempt < MaxToolCalls; attempt++ {
		_ = s.ExecuteTool(InspectVideoRangeInput{StartMS: -1, EndMS: 1, IntervalMS: 500})
	}
	if s.RemainingToolCalls() != 0 {
		t.Fatalf("remaining calls = %d, want 0", s.RemainingToolCalls())
	}
}

func TestCancelledSessionStopsAndCleansTemporaryFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newSession(ctx, Range{StartMS: 0, EndMS: 10_000}, "video", func() {}, cancellationFrameExtractor{})
	if err != nil {
		t.Fatal(err)
	}
	workDir := s.workDir
	cancel()
	if err := s.InitialSample(); !errors.Is(err, context.Canceled) {
		t.Fatalf("InitialSample() error = %v, want context.Canceled", err)
	}
	s.Close()
	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary frame directory still exists: %v", err)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		value, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = value.Unwrap()
	}
	return false
}
