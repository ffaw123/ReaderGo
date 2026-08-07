package worker

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// 该版本包含截至接入时已公开命令注入问题的修复；旧版本不能处理不受信任的页面 URL。
	minimumYTDLPVersion = "2026.07.04"
	ytdlpOutputLimit    = 16 * 1024
	ytdlpDiskPoll       = 250 * time.Millisecond
)

var (
	ytdlpVersionPattern = regexp.MustCompile(`\b\d{4}\.\d{2}\.\d{2}\b`)
	ytdlpURLPattern     = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

type preparePageSource uint8

const (
	preparePageNone preparePageSource = iota
	preparePageBilibili
)

type routingPrepareDownloader struct {
	direct *prepareDownloader
	ytdlp  *ytdlpAdapter
}

// Download 只把明确的 Bilibili 单视频页面交给 yt-dlp；其他 URL 保持原有直链安全下载流程。
func (d routingPrepareDownloader) Download(ctx context.Context, rawURL, destination string) (int64, error) {
	source, matchedHost, err := classifyYTDLPPage(rawURL)
	if err != nil {
		return 0, err
	}
	if matchedHost {
		return d.ytdlp.Download(ctx, source, rawURL, destination)
	}
	return d.direct.Download(ctx, rawURL, destination)
}

type ytdlpAdapter struct {
	executable string
	ffmpegPath string
	prefixArgs []string
	validator  *prepareDownloader
	options    PrepareOptions
}

func newYTDLPAdapter(path, ffmpegPath string, validator *prepareDownloader, options PrepareOptions) (*ytdlpAdapter, error) {
	if validator == nil {
		return nil, fmt.Errorf("yt-dlp adapter requires URL validator")
	}
	if strings.TrimSpace(ffmpegPath) == "" {
		return nil, fmt.Errorf("yt-dlp adapter requires ffmpeg path")
	}
	executable, err := resolveYTDLP(path)
	if err != nil {
		return nil, err
	}
	return &ytdlpAdapter{executable: executable, ffmpegPath: ffmpegPath, validator: validator, options: options}, nil
}

func resolveYTDLP(path string) (string, error) {
	resolved, err := exec.LookPath(strings.TrimSpace(path))
	if err != nil {
		return "", fmt.Errorf("resolve YT_DLP_PATH: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve absolute yt-dlp path: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, resolved, "--ignore-config", "--no-plugin-dirs", "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("validate yt-dlp executable: %w", err)
	}
	version, err := parseYTDLPVersion(string(output))
	if err != nil {
		return "", err
	}
	minimum, _ := time.Parse("2006.01.02", minimumYTDLPVersion)
	if version.Before(minimum) {
		return "", fmt.Errorf("yt-dlp %s is too old; version %s or newer is required", version.Format("2006.01.02"), minimumYTDLPVersion)
	}
	return resolved, nil
}

func parseYTDLPVersion(output string) (time.Time, error) {
	value := ytdlpVersionPattern.FindString(output)
	if value == "" {
		return time.Time{}, fmt.Errorf("yt-dlp returned an unrecognized version %q", strings.TrimSpace(output))
	}
	version, err := time.Parse("2006.01.02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse yt-dlp version %q: %w", value, err)
	}
	return version, nil
}

func classifyYTDLPPage(rawURL string) (preparePageSource, bool, error) {
	target, err := parsePrepareURL(rawURL)
	if err != nil {
		return preparePageNone, false, err
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	path := strings.TrimRight(target.EscapedPath(), "/")
	if isYouTubeHost(host) || isDouyinHost(host) || isXiaoHongShuHost(host) {
		// 已移除的平台必须在这里明确拒绝，不能回退到普通直链下载并把 HTML 当作视频。
		return preparePageNone, true, prepareInput(fmt.Errorf("video page host %q is not supported", host))
	}
	if target.Scheme != "https" {
		if isBilibiliHost(host) {
			return preparePageBilibili, true, prepareInput(fmt.Errorf("Bilibili page URL must use HTTPS"))
		}
	}
	if target.Port() != "" && isBilibiliHost(host) {
		return preparePageNone, true, prepareInput(fmt.Errorf("video page URLs must use the default HTTPS port"))
	}

	switch host {
	case "bilibili.com", "www.bilibili.com", "m.bilibili.com":
		bangumiID := strings.TrimPrefix(path, "/bangumi/play/")
		if hasSinglePathID(path, "/video/") || (hasSinglePathID(path, "/bangumi/play/") && strings.HasPrefix(bangumiID, "ep") && isASCIIInteger(strings.TrimPrefix(bangumiID, "ep"))) {
			return preparePageBilibili, true, nil
		}
		return preparePageBilibili, true, prepareInput(fmt.Errorf("Bilibili URL must identify a single video or bangumi episode"))
	case "b23.tv", "www.b23.tv":
		// 禁用需要 Generic extractor 才能展开的短链，避免为了便利重新放开任意站点解析能力。
		return preparePageBilibili, true, prepareInput(fmt.Errorf("Bilibili short URLs are not supported; use the full bilibili.com video URL"))
	default:
		if strings.HasSuffix(host, ".bilibili.com") || strings.HasSuffix(host, ".b23.tv") {
			return preparePageBilibili, true, prepareInput(fmt.Errorf("Bilibili host %q is not allowed", host))
		}
		return preparePageNone, false, nil
	}
}

func hasSinglePathID(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	return id != "" && !strings.Contains(id, "/")
}

func isASCIIInteger(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func isYouTubeHost(host string) bool {
	return host == "youtu.be" || host == "youtube.com" || host == "youtube-nocookie.com" || strings.HasSuffix(host, ".youtube.com") || strings.HasSuffix(host, ".youtube-nocookie.com")
}

func isBilibiliHost(host string) bool {
	return host == "b23.tv" || host == "bilibili.com" || strings.HasSuffix(host, ".b23.tv") || strings.HasSuffix(host, ".bilibili.com")
}

func isDouyinHost(host string) bool {
	return host == "douyin.com" || strings.HasSuffix(host, ".douyin.com")
}

func isXiaoHongShuHost(host string) bool {
	return host == "xiaohongshu.com" || host == "xhslink.com" || strings.HasSuffix(host, ".xiaohongshu.com") || strings.HasSuffix(host, ".xhslink.com")
}

func (a *ytdlpAdapter) Download(ctx context.Context, source preparePageSource, rawURL, destination string) (int64, error) {
	target, err := parsePrepareURL(rawURL)
	if err != nil {
		return 0, err
	}
	target = normalizeYTDLPTarget(source, target)
	if err := a.validator.validateTarget(ctx, target); err != nil {
		return 0, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, a.options.DownloadTimeout)
	defer cancel()

	args := append([]string{}, a.prefixArgs...)
	args = append(args, a.commandArgs(target.String(), destination)...)
	command := exec.CommandContext(downloadCtx, a.executable, args...)
	command.Dir = filepath.Dir(destination)
	command.Stdin = nil
	command.WaitDelay = 5 * time.Second
	stdout, stderr := &limitedCommandOutput{limit: ytdlpOutputLimit}, &limitedCommandOutput{limit: ytdlpOutputLimit}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start yt-dlp: %w", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	ticker := time.NewTicker(ytdlpDiskPoll)
	defer ticker.Stop()
	temporarySizeLimit := ytdlpTemporarySizeLimit(a.options.MaxFileSize)
	for {
		select {
		case waitErr := <-waited:
			if waitErr != nil {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				if downloadCtx.Err() != nil {
					return 0, fmt.Errorf("yt-dlp exceeded download timeout: %w", downloadCtx.Err())
				}
				return 0, classifyYTDLPCommandError(waitErr, stderr.String(), target.String())
			}
			return finalizeYTDLPOutput(destination, a.options.MaxFileSize)
		case <-ticker.C:
			size, err := prepareDirectorySize(command.Dir)
			if err != nil {
				_ = command.Process.Kill()
				<-waited
				return 0, fmt.Errorf("measure yt-dlp temporary files: %w", err)
			}
			if size > temporarySizeLimit {
				_ = command.Process.Kill()
				<-waited
				return 0, prepareInput(fmt.Errorf("yt-dlp temporary files exceed resource limit of %d bytes", temporarySizeLimit))
			}
		case <-downloadCtx.Done():
			_ = command.Process.Kill()
			<-waited
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("yt-dlp exceeded download timeout: %w", downloadCtx.Err())
		}
	}
}

func normalizeYTDLPTarget(source preparePageSource, target *url.URL) *url.URL {
	normalized := *target
	if source == preparePageBilibili && strings.EqualFold(target.Hostname(), "m.bilibili.com") {
		normalized.Host = "www.bilibili.com"
	}
	return &normalized
}

func (a *ytdlpAdapter) commandArgs(rawURL, destination string) []string {
	maxDurationSeconds := int64(math.Ceil(a.options.MaxDuration.Seconds()))
	socketTimeoutSeconds := max(1, int(math.Ceil(a.options.ConnectTimeout.Seconds())))
	return []string{
		"--ignore-config", "--no-plugin-dirs", "--no-remote-components", "--no-exec", "--no-cache-dir",
		"--no-cookies", "--no-cookies-from-browser", "--proxy", "", "--default-search", "error",
		"--ies", "BiliBili,BiliBiliBangumi,end", "--no-playlist", "--abort-on-error",
		"--no-write-comments", "--no-write-subs", "--no-write-auto-subs", "--no-write-thumbnail",
		"--no-write-info-json", "--no-write-description", "--no-write-playlist-metafiles",
		"--no-keep-fragments", "--abort-on-unavailable-fragments", "--fixup", "never", "--downloader", "dash,m3u8:native",
		"--ffmpeg-location", a.ffmpegPath, "--merge-output-format", "mp4/mkv",
		"--socket-timeout", strconv.Itoa(socketTimeoutSeconds),
		"--retries", "2", "--fragment-retries", "2", "--extractor-retries", "2", "--max-filesize", strconv.FormatInt(a.options.MaxFileSize, 10),
		"--match-filters", fmt.Sprintf("!is_live & duration<=?%d", maxDurationSeconds),
		"--format", fmt.Sprintf("bestvideo*[height<=?%d]+bestaudio/best[height<=?%d][vcodec!=none]", a.options.YTDLPMaxHeight, a.options.YTDLPMaxHeight),
		"--output", destination + ".%(ext)s", "--quiet", "--no-warnings", "--no-progress", "--", rawURL,
	}
}

func ytdlpTemporarySizeLimit(maxFileSize int64) int64 {
	if maxFileSize > math.MaxInt64/3 {
		return math.MaxInt64
	}
	// 合并阶段会同时保留音视频输入、最终输出及少量分片元数据，仍以最终文件上限的三倍封顶。
	return maxFileSize * 3
}

func prepareDirectorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func finalizeYTDLPOutput(destination string, maxSize int64) (int64, error) {
	directory, base := filepath.Dir(destination), filepath.Base(destination)+"."
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, fmt.Errorf("list yt-dlp output: %w", err)
	}
	var output string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), base) && !strings.HasSuffix(entry.Name(), ".part") && !strings.HasSuffix(entry.Name(), ".ytdl") {
			if output != "" {
				return 0, fmt.Errorf("yt-dlp produced more than one media file")
			}
			output = filepath.Join(directory, entry.Name())
		}
	}
	if output == "" {
		return 0, fmt.Errorf("yt-dlp completed without a media file")
	}
	info, err := os.Stat(output)
	if err != nil {
		return 0, fmt.Errorf("stat yt-dlp output: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maxSize {
		return 0, prepareInput(fmt.Errorf("yt-dlp output has invalid size %d", info.Size()))
	}
	if err := os.Rename(output, destination); err != nil {
		return 0, fmt.Errorf("finalize yt-dlp output: %w", err)
	}
	return info.Size(), nil
}

func classifyYTDLPCommandError(commandErr error, stderr, rawURL string) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = commandErr.Error()
	}
	// 错误状态会回传给用户，不能把查询串中可能存在的临时签名原样写入数据库。
	message = strings.ReplaceAll(message, rawURL, "[video URL]")
	message = ytdlpURLPattern.ReplaceAllString(message, "[URL]")
	message = conciseYTDLPError(message)
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"timed out", "temporary failure", "connection reset", "connection refused", "network is unreachable",
		"remote end closed", "http error 408", "http error 409", "http error 425", "http error 429",
		"http error 500", "http error 502", "http error 503", "http error 504",
	} {
		if strings.Contains(lower, marker) {
			return fmt.Errorf("yt-dlp transient failure: %s", message)
		}
	}
	for _, marker := range []string{"no such option", "unrecognized option", "unknown option", "usage: yt-dlp"} {
		if strings.Contains(lower, marker) {
			// 命令行契约不兼容属于部署问题，重试或让用户更换 URL 都无法恢复，应直接进入 DLQ 排查。
			return permanentProcessError(fmt.Errorf("yt-dlp command is incompatible: %s", message))
		}
	}
	return prepareInput(fmt.Errorf("yt-dlp rejected the video URL: %s", message))
}

func conciseYTDLPError(message string) string {
	lines := strings.Split(strings.TrimSpace(message), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if strings.Contains(strings.ToLower(line), "error:") {
			return line
		}
	}
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return "unknown yt-dlp error"
}

type limitedCommandOutput struct {
	data  []byte
	limit int
}

func (w *limitedCommandOutput) Write(data []byte) (int, error) {
	original := len(data)
	if w.limit <= 0 {
		return original, nil
	}
	if len(data) >= w.limit {
		w.data = append(w.data[:0], data[len(data)-w.limit:]...)
		return original, nil
	}
	if overflow := len(w.data) + len(data) - w.limit; overflow > 0 {
		copy(w.data, w.data[overflow:])
		w.data = w.data[:len(w.data)-overflow]
	}
	w.data = append(w.data, data...)
	return original, nil
}

func (w *limitedCommandOutput) String() string { return string(w.data) }
