// Package media 封装 Worker 所需的 FFmpeg 本地媒体处理能力。
package media

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// FFmpeg 是一个可复用的 FFmpeg/ffprobe 命令封装。
// 所有命令都通过 os/exec 传参，不经 Shell，避免文件路径和参数被解释执行。
type FFmpeg struct {
	path    string
	ffprobe string
}

// MediaInfo 是视频准备、ASR 和 Vision 共用的基础媒体信息。
// DurationMS 以视频开始为 0 的全局时间基准表示，和 Kafka 设计中的时间契约一致。
type MediaInfo struct {
	DurationMS int64
	FormatName string
	Width      int
	Height     int
	VideoCodec string
	AudioCodec string
}

// SilenceInterval 表示原视频时间轴上的静音区间。
type SilenceInterval struct {
	StartMS int64
	EndMS   int64
}

// NewFFmpeg 创建 FFmpeg 工具，并校验 ffmpeg 与 ffprobe 是否可以定位。
// FFMPEG_PATH 可以是绝对路径，也可以是已加入 PATH 的可执行文件名。
func NewFFmpeg(path string) (*FFmpeg, error) {
	ffmpegPath, err := resolveExecutable(path)
	if err != nil {
		return nil, fmt.Errorf("resolve FFMPEG_PATH: %w", err)
	}

	ffprobePath, err := resolveFFprobe(ffmpegPath)
	if err != nil {
		return nil, err
	}
	return &FFmpeg{path: ffmpegPath, ffprobe: ffprobePath}, nil
}

// Path 返回已解析的 ffmpeg 可执行文件绝对路径，便于启动日志排查。
func (f *FFmpeg) Path() string { return f.path }

// Validate 执行版本命令，确认当前进程实际有权限运行 FFmpeg。
func (f *FFmpeg) Validate(ctx context.Context) error {
	if _, err := f.run(ctx, f.path, "-version"); err != nil {
		return fmt.Errorf("run ffmpeg at %q: %w", f.path, err)
	}
	if _, err := f.run(ctx, f.ffprobe, "-version"); err != nil {
		return fmt.Errorf("run ffprobe at %q: %w", f.ffprobe, err)
	}
	return nil
}

// Probe 读取视频时长、尺寸与编解码信息，不解码完整视频。
func (f *FFmpeg) Probe(ctx context.Context, inputPath string) (MediaInfo, error) {
	output, err := f.run(ctx, f.ffprobe,
		"-v", "error",
		"-show_entries", "format=duration,format_name:stream=codec_type,codec_name,width,height",
		"-of", "json",
		inputPath,
	)
	if err != nil {
		return MediaInfo{}, fmt.Errorf("probe media %q: %w", inputPath, err)
	}

	var result struct {
		Format struct {
			Duration   string `json:"duration"`
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return MediaInfo{}, fmt.Errorf("parse ffprobe response: %w", err)
	}
	durationSeconds, err := strconv.ParseFloat(result.Format.Duration, 64)
	if err != nil || durationSeconds < 0 {
		return MediaInfo{}, fmt.Errorf("ffprobe returned invalid duration %q", result.Format.Duration)
	}

	info := MediaInfo{
		DurationMS: int64(math.Round(durationSeconds * 1000)),
		FormatName: result.Format.FormatName,
	}
	for _, stream := range result.Streams {
		switch stream.CodecType {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec, info.Width, info.Height = stream.CodecName, stream.Width, stream.Height
			}
		case "audio":
			if info.AudioCodec == "" {
				info.AudioCodec = stream.CodecName
			}
		}
	}
	return info, nil
}

// ExtractAudio 将完整视频转换为 16 kHz、单声道 PCM WAV。
// 该输出适合交给 ASR；它从视频 0 ms 开始，因此 ASR 返回的毫秒区间可直接写入全局时间轴。
func (f *FFmpeg) ExtractAudio(ctx context.Context, inputPath, outputPath string) error {
	return f.extractAudio(ctx, inputPath, outputPath, nil, nil)
}

// ExtractAudioSegment 提取 [start, end) 区间的音频，两个时间点均相对视频 0 ms。
// 调用方需要将 ASR 段结果加回 start，才能保持全局时间基准。
func (f *FFmpeg) ExtractAudioSegment(ctx context.Context, inputPath, outputPath string, start, end time.Duration) error {
	if start < 0 || end <= start {
		return fmt.Errorf("audio segment must satisfy 0 <= start < end")
	}
	return f.extractAudio(ctx, inputPath, outputPath, &start, &end)
}

// DetectSilences 检测低于阈值且持续达到指定时长的静音区间。
func (f *FFmpeg) DetectSilences(ctx context.Context, inputPath string, duration time.Duration, thresholdDB int, minSilence time.Duration) ([]SilenceInterval, error) {
	filter := fmt.Sprintf("silencedetect=noise=%ddB:d=%s", thresholdDB, ffmpegTimestamp(minSilence))
	output, err := f.run(ctx, f.path, "-nostdin", "-i", inputPath, "-af", filter, "-f", "null", "-")
	if err != nil {
		return nil, fmt.Errorf("detect silence in %q: %w", inputPath, err)
	}
	return parseSilenceIntervals(string(output), duration), nil
}

func parseSilenceIntervals(output string, duration time.Duration) []SilenceInterval {
	var intervals []SilenceInterval
	var startMS int64 = -1
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := silenceValue(line, "silence_start:"); ok {
			startMS = max(0, value)
		}
		if value, ok := silenceValue(line, "silence_end:"); ok && startMS >= 0 {
			endMS := min(value, duration.Milliseconds())
			if endMS > startMS {
				intervals = append(intervals, SilenceInterval{StartMS: startMS, EndMS: endMS})
			}
			startMS = -1
		}
	}
	if startMS >= 0 && duration.Milliseconds() > startMS {
		intervals = append(intervals, SilenceInterval{StartMS: startMS, EndMS: duration.Milliseconds()})
	}
	return intervals
}

func silenceValue(line, marker string) (int64, bool) {
	index := strings.Index(line, marker)
	if index < 0 {
		return 0, false
	}
	value := strings.Fields(line[index+len(marker):])
	if len(value) == 0 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(value[0], 64)
	if err != nil {
		return 0, false
	}
	return int64(math.Round(seconds * 1000)), true
}

func (f *FFmpeg) extractAudio(ctx context.Context, inputPath, outputPath string, start, end *time.Duration) error {
	args := []string{"-nostdin", "-y", "-i", inputPath}
	if start != nil {
		// 精确截取放在 -i 之后，避免关键帧寻址造成实际内容时间偏移。
		args = append(args, "-ss", ffmpegTimestamp(*start), "-t", ffmpegTimestamp(*end-*start))
	}
	args = append(args, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", outputPath)
	if _, err := f.run(ctx, f.path, args...); err != nil {
		return fmt.Errorf("extract audio from %q: %w", inputPath, err)
	}
	return nil
}

// ExtractFrameAt 在相对视频 0 ms 的 timestamp 处抽取一张 JPEG 帧。
// -ss 位于输入参数之后以优先保证 Vision 所用画面与记录时间点一致。
func (f *FFmpeg) ExtractFrameAt(ctx context.Context, inputPath, outputPath string, timestamp time.Duration) error {
	if timestamp < 0 {
		return fmt.Errorf("frame timestamp must not be negative")
	}
	if _, err := f.run(ctx, f.path,
		"-nostdin", "-y", "-i", inputPath,
		"-ss", ffmpegTimestamp(timestamp),
		"-frames:v", "1", "-an", "-q:v", "2", outputPath,
	); err != nil {
		return fmt.Errorf("extract frame at %s from %q: %w", timestamp, inputPath, err)
	}
	return nil
}

// ExtractFramesEvery 按固定间隔抽帧，输出路径必须包含 FFmpeg 图像序列模式（例如 frame_%06d.jpg）。
// 第 n 帧对应的时间点由调用方按 n * interval 计算；镜头切分等非固定采样可改用 ExtractFrameAt。
func (f *FFmpeg) ExtractFramesEvery(ctx context.Context, inputPath, outputPattern string, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("frame interval must be positive")
	}
	fps := 1 / interval.Seconds()
	if _, err := f.run(ctx, f.path,
		"-nostdin", "-y", "-i", inputPath,
		"-vf", "fps="+strconv.FormatFloat(fps, 'f', -1, 64),
		"-an", "-q:v", "2", outputPattern,
	); err != nil {
		return fmt.Errorf("extract frames from %q: %w", inputPath, err)
	}
	return nil
}

func (f *FFmpeg) run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	output, err := command.CombinedOutput()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s", err, message)
}

func resolveExecutable(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(path) || filepath.Dir(path) != "." {
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(absolutePath)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", fmt.Errorf("%q is a directory", absolutePath)
		}
		return absolutePath, nil
	}
	return exec.LookPath(path)
}

func resolveFFprobe(ffmpegPath string) (string, error) {
	fileName := "ffprobe"
	if runtime.GOOS == "windows" || strings.EqualFold(filepath.Ext(ffmpegPath), ".exe") {
		fileName += ".exe"
	}
	// 优先使用 FFmpeg 同一发行包内的 ffprobe，确保二者版本兼容。
	siblingPath := filepath.Join(filepath.Dir(ffmpegPath), fileName)
	if info, err := os.Stat(siblingPath); err == nil && !info.IsDir() {
		return siblingPath, nil
	}
	probePath, err := exec.LookPath(fileName)
	if err != nil {
		return "", fmt.Errorf("ffprobe was not found beside ffmpeg at %q or on PATH", siblingPath)
	}
	return probePath, nil
}

func ffmpegTimestamp(value time.Duration) string {
	// FFmpeg 接受秒数小数；固定保留毫秒精度以符合项目的时间数据契约。
	return strconv.FormatFloat(value.Seconds(), 'f', 3, 64)
}
