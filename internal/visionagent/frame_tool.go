package visionagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type session struct {
	ctx        context.Context
	allowed    Range
	videoPath  string
	release    func()
	workDir    string
	ffmpeg     frameExtractor
	frames     map[int64]Frame
	toolCalls  int
	toolErrors []string
}

type frameExtractor interface {
	ExtractFrameAt(context.Context, string, string, time.Duration) error
}

func newSession(ctx context.Context, allowed Range, videoPath string, release func(), ffmpeg frameExtractor) (*session, error) {
	workDir, err := os.MkdirTemp("", "readergo-vision-frames-*")
	if err != nil {
		release()
		return nil, fmt.Errorf("create vision frame directory: %w", err)
	}
	return &session{
		ctx: ctx, allowed: allowed, videoPath: videoPath, release: release,
		workDir: workDir, ffmpeg: ffmpeg, frames: make(map[int64]Frame),
	}, nil
}

func (s *session) Close() {
	_ = os.RemoveAll(s.workDir)
	if s.release != nil {
		s.release()
	}
}

func (s *session) InitialSample() error {
	return s.sample(InspectVideoRangeInput{StartMS: s.allowed.StartMS, EndMS: s.allowed.EndMS, IntervalMS: InitialIntervalMS})
}

func (s *session) ExecuteTool(input InspectVideoRangeInput) error {
	// 无效调用同样消耗次数，避免模型通过反复提交错误参数形成无限循环。
	s.toolCalls++
	if s.toolCalls > MaxToolCalls {
		return ErrToolBudget
	}
	if !within(s.allowed, input) {
		return fmt.Errorf("requested range is outside the user-authorized range: %w", ErrRangeOutOfBounds)
	}
	return s.sample(input)
}

func (s *session) sample(input InspectVideoRangeInput) error {
	timestamps, err := frameTimestamps(input)
	if err != nil {
		return err
	}
	newTimestamps := make([]int64, 0, len(timestamps))
	for _, timestamp := range timestamps {
		if _, exists := s.frames[timestamp]; !exists {
			newTimestamps = append(newTimestamps, timestamp)
		}
	}
	if len(s.frames)+len(newTimestamps) > MaxUniqueFrames {
		return fmt.Errorf("tool would add %d frames with %d remaining: %w", len(newTimestamps), MaxUniqueFrames-len(s.frames), ErrFrameBudget)
	}
	for _, timestamp := range newTimestamps {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(s.workDir, fmt.Sprintf("frame-%012d.jpg", timestamp))
		if err := s.ffmpeg.ExtractFrameAt(s.ctx, s.videoPath, path, time.Duration(timestamp)*time.Millisecond); err != nil {
			return err
		}
		s.frames[timestamp] = Frame{TimestampMS: timestamp, DisplayTime: formatTimestamp(timestamp), Path: path}
	}
	return nil
}

func (s *session) Frames() []Frame {
	timestamps := make([]int64, 0, len(s.frames))
	for timestamp := range s.frames {
		timestamps = append(timestamps, timestamp)
	}
	slices.Sort(timestamps)
	frames := make([]Frame, len(timestamps))
	for index, timestamp := range timestamps {
		frames[index] = s.frames[timestamp]
	}
	return frames
}

func (s *session) AppendToolError(err error) {
	if err != nil {
		s.toolErrors = append(s.toolErrors, err.Error())
	}
}

func (s *session) RemainingToolCalls() int { return max(0, MaxToolCalls-s.toolCalls) }
func (s *session) RemainingFrames() int    { return max(0, MaxUniqueFrames-len(s.frames)) }
