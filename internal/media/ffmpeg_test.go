package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFFmpegTimestampUsesMilliseconds(t *testing.T) {
	tests := []struct {
		value time.Duration
		want  string
	}{
		{value: 0, want: "0.000"},
		{value: 12*time.Second + 345*time.Millisecond, want: "12.345"},
		{value: 1500 * time.Millisecond, want: "1.500"},
	}
	for _, test := range tests {
		if got := ffmpegTimestamp(test.value); got != test.want {
			t.Errorf("ffmpegTimestamp(%s) = %s, want %s", test.value, got, test.want)
		}
	}
}

func TestParseSilenceIntervals(t *testing.T) {
	output := "silence_start: 8.75\nsilence_end: 9.25 | silence_duration: 0.5\nsilence_start: 19.6\n"
	got := parseSilenceIntervals(output, 20*time.Second)
	want := []SilenceInterval{{StartMS: 8750, EndMS: 9250}, {StartMS: 19600, EndMS: 20000}}
	if len(got) != len(want) {
		t.Fatalf("interval count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("interval %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestFFmpegSmoke 会在显式设置 FFMPEG_SMOKE_PATH 后验证本机工具的完整调用链。
// 默认跳过，保证 CI 不需要预装 FFmpeg；本地可借此验证实际安装路径。
func TestFFmpegSmoke(t *testing.T) {
	path := os.Getenv("FFMPEG_SMOKE_PATH")
	if path == "" {
		t.Skip("set FFMPEG_SMOKE_PATH to run the local FFmpeg smoke test")
	}
	tool, err := NewFFmpeg(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")
	if _, err := tool.run(ctx, tool.path,
		"-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=8000",
		"-t", "1.2", "-c:v", "mpeg4", "-c:a", "aac", input,
	); err != nil {
		t.Fatalf("create media fixture: %v", err)
	}
	info, err := tool.Probe(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if info.DurationMS < 1000 || info.Width != 64 || info.Height != 48 {
		t.Fatalf("unexpected media info: %+v", info)
	}
	for _, output := range []string{filepath.Join(dir, "audio.wav"), filepath.Join(dir, "frame.jpg")} {
		var err error
		if filepath.Ext(output) == ".wav" {
			err = tool.ExtractAudio(ctx, input, output)
		} else {
			err = tool.ExtractFrameAt(ctx, input, output, 500*time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		fileInfo, err := os.Stat(output)
		if err != nil || fileInfo.Size() == 0 {
			t.Fatalf("expected non-empty media output %q, err=%v", output, err)
		}
	}
}
