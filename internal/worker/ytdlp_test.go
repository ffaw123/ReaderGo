package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClassifyYTDLPPageAllowsOnlySupportedSingleVideoPages(t *testing.T) {
	tests := []struct {
		url    string
		source preparePageSource
	}{
		{url: "https://www.bilibili.com/video/BV1xx411c7mD", source: preparePageBilibili},
		{url: "https://m.bilibili.com/bangumi/play/ep123", source: preparePageBilibili},
	}
	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			source, matched, err := classifyYTDLPPage(test.url)
			if err != nil || !matched || source != test.source {
				t.Fatalf("classifyYTDLPPage() = (%d, %t, %v)", source, matched, err)
			}
		})
	}
}

func TestClassifyYTDLPPageRejectsNonVideoPages(t *testing.T) {
	for _, rawURL := range []string{
		"https://www.youtube.com/watch?v=video-id",
		"https://youtu.be/video-id",
		"https://www.youtube.com/channel/channel-id",
		"https://space.bilibili.com/12345",
		"https://www.bilibili.com/v/popular/all",
		"https://www.bilibili.com/bangumi/play/ss123",
		"https://b23.tv/",
		"https://b23.tv/short-id",
		"http://www.bilibili.com/video/BV1xx411c7mD",
		"https://www.douyin.com/video/6961737553342991651",
		"https://v.douyin.com/short-id/",
		"https://www.douyin.com/user/user-id",
		"https://www.douyin.com/video/not-a-number",
		"http://www.douyin.com/video/6961737553342991651",
		"https://live.douyin.com/12345",
		"https://xhslink.com/short-id",
		"https://www.xiaohongshu.com/red_video/6a6de4c50000000025004472?xsec_token=test-token",
		"https://www.xiaohongshu.com/explore/6411cf99000000001300b6d9",
		"https://xiaohongshu.com/discovery/item/674051740000000007027a15?xsec_token=test-token",
		"https://www.xiaohongshu.com/user/profile/user-id",
		"https://www.xiaohongshu.com/red_video/not-hex",
		"https://www.xiaohongshu.com/explore/not-hex",
		"http://www.xiaohongshu.com/explore/6411cf99000000001300b6d9",
		"https://www.xiaohongshu.com:8443/explore/6411cf99000000001300b6d9",
	} {
		_, matched, err := classifyYTDLPPage(rawURL)
		if !matched || !isPrepareInputError(err) {
			t.Errorf("classifyYTDLPPage(%q) = (matched=%t, err=%v)", rawURL, matched, err)
		}
	}
	if source, matched, err := classifyYTDLPPage("https://cdn.example.com/video.mp4"); err != nil || matched || source != preparePageNone {
		t.Fatalf("direct URL classification = (%d, %t, %v)", source, matched, err)
	}
}

func TestNormalizeYTDLPTargetUsesExtractorSupportedHosts(t *testing.T) {
	tests := []struct {
		source preparePageSource
		input  string
		want   string
	}{
		{source: preparePageBilibili, input: "https://m.bilibili.com/video/BV1xx411c7mD", want: "https://www.bilibili.com/video/BV1xx411c7mD"},
	}
	for _, test := range tests {
		target, err := parsePrepareURL(test.input)
		if err != nil {
			t.Fatal(err)
		}
		if got := normalizeYTDLPTarget(test.source, target).String(); got != test.want {
			t.Errorf("normalizeYTDLPTarget(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestYTDLPCommandArgsDisableAmbientCapabilitiesAndLimitResources(t *testing.T) {
	options := testPrepareOptions(4096)
	adapter := &ytdlpAdapter{options: options}
	rawURL := "https://www.bilibili.com/video/BV1xx411c7mD"
	args := adapter.commandArgs(rawURL, filepath.Join(t.TempDir(), "source"))
	for _, expected := range []string{
		"--ignore-config", "--no-plugin-dirs", "--no-remote-components", "--no-exec", "--no-cache-dir",
		"--no-cookies", "--no-cookies-from-browser", "--no-playlist", "--max-filesize", "--match-filters", "--format", "--fixup", "--downloader",
	} {
		if !slices.Contains(args, expected) {
			t.Errorf("yt-dlp args do not contain %q: %#v", expected, args)
		}
	}
	if got := valueAfter(args, "--ies"); got != "BiliBili,BiliBiliBangumi,end" {
		t.Errorf("extractors = %q", got)
	}
	if got := valueAfter(args, "--proxy"); got != "" {
		t.Errorf("proxy = %q, want direct connection", got)
	}
	if format := valueAfter(args, "--format"); !strings.Contains(format, "+bestaudio") {
		t.Errorf("format = %q, want separate audio fallback for Bilibili", format)
	}
	if len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != rawURL {
		t.Fatalf("URL is not isolated after --: %#v", args[len(args)-2:])
	}
	if slices.Contains(args, "--no-write-link") {
		t.Fatal("yt-dlp args contain unsupported --no-write-link option")
	}
}

func TestYTDLPAdapterDownloadsToControlledDestination(t *testing.T) {
	t.Setenv("READERGO_YTDLP_HELPER", "success")
	argsPath := filepath.Join(t.TempDir(), "args.json")
	t.Setenv("READERGO_YTDLP_ARGS", argsPath)
	workDir := t.TempDir()
	adapter := testYTDLPAdapter(t, testPrepareOptions(1024))
	destination := filepath.Join(workDir, "source-video")
	rawURL := "https://www.bilibili.com/video/BV1xx411c7mD"
	size, err := adapter.Download(context.Background(), preparePageBilibili, rawURL, destination)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) || string(content) != "yt-dlp-media" {
		t.Fatalf("download = (%d, %q)", size, content)
	}
	var args []string
	encoded, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &args); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--ignore-config") || args[len(args)-1] != rawURL {
		t.Fatalf("helper args = %#v", args)
	}
}

func TestYTDLPAdapterKillsOversizedDownload(t *testing.T) {
	t.Setenv("READERGO_YTDLP_HELPER", "oversize")
	options := testPrepareOptions(4)
	adapter := testYTDLPAdapter(t, options)
	started := time.Now()
	_, err := adapter.Download(context.Background(), preparePageBilibili, "https://www.bilibili.com/video/BV1xx411c7mD", filepath.Join(t.TempDir(), "source-video"))
	if !isPrepareInputError(err) || !strings.Contains(err.Error(), "resource limit") {
		t.Fatalf("oversize error = %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("oversized yt-dlp process was not terminated promptly")
	}
}

func TestYTDLPAdapterCancellationTerminatesProcess(t *testing.T) {
	t.Setenv("READERGO_YTDLP_HELPER", "wait")
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("READERGO_YTDLP_MARKER", marker)
	adapter := testYTDLPAdapter(t, testPrepareOptions(1024))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	destination := filepath.Join(t.TempDir(), "source-video")
	go func() {
		_, err := adapter.Download(ctx, preparePageBilibili, "https://www.bilibili.com/video/BV1xx411c7mD", destination)
		result <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("yt-dlp helper did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("yt-dlp process was not terminated after cancellation")
	}
}

func TestClassifyYTDLPCommandErrorSeparatesTransientAndUserFailures(t *testing.T) {
	rawURL := "https://www.bilibili.com/video/BV1xx411c7mD?token=secret"
	transient := classifyYTDLPCommandError(errors.New("exit status 1"), "ERROR: HTTP Error 503", rawURL)
	if isPrepareInputError(transient) {
		t.Fatalf("transient error was classified as user input: %v", transient)
	}
	input := classifyYTDLPCommandError(errors.New("exit status 1"), "ERROR: video unavailable at "+rawURL, rawURL)
	if !isPrepareInputError(input) || strings.Contains(input.Error(), "token=secret") {
		t.Fatalf("user error classification/redaction = %v", input)
	}
}

func TestClassifyYTDLPCommandErrorPreservesParserCause(t *testing.T) {
	stderr := "Usage: yt-dlp.exe [OPTIONS] URL [URL...]\n\nyt-dlp.exe: error: no such option: --no-write-link"
	err := classifyYTDLPCommandError(errors.New("exit status 2"), stderr, "https://www.bilibili.com/video/BV1xx411c7mD")
	if action := decideFailureAction(err, 1, 3); action != failureActionFailWithDeadLetter {
		t.Fatalf("parser failure action = %v, err = %v", action, err)
	}
	if !strings.Contains(err.Error(), "no such option: --no-write-link") || strings.Contains(err.Error(), "Usage:") {
		t.Fatalf("parser failure was not reduced to its cause: %v", err)
	}
}

func TestLimitedCommandOutputRetainsTail(t *testing.T) {
	output := &limitedCommandOutput{limit: len("\nerror: bad")}
	for _, part := range []string{"Usage: very long help", "\nerror: bad"} {
		if _, err := output.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if got := output.String(); got != "\nerror: bad" {
		t.Fatalf("retained output = %q", got)
	}
}

func TestParseYTDLPVersionRequiresDateVersion(t *testing.T) {
	version, err := parseYTDLPVersion("stable@2026.07.04")
	if err != nil || version.Format("2006.01.02") != minimumYTDLPVersion {
		t.Fatalf("version = %v, err = %v", version, err)
	}
	if _, err := parseYTDLPVersion("unknown"); err == nil {
		t.Fatal("unrecognized version was accepted")
	}
}

func testYTDLPAdapter(t *testing.T, options PrepareOptions) *ytdlpAdapter {
	t.Helper()
	resolver := &staticPrepareResolver{addresses: map[string][]net.IPAddr{
		"www.bilibili.com": {{IP: net.ParseIP("1.1.1.1")}},
	}}
	validator, err := newPrepareDownloader(options, resolver, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("yt-dlp URL validation must not dial directly")
	})
	if err != nil {
		t.Fatal(err)
	}
	return &ytdlpAdapter{
		executable: os.Args[0],
		ffmpegPath: "ffmpeg",
		prefixArgs: []string{"-test.run=TestYTDLPHelperProcess", "--"},
		validator:  validator,
		options:    options,
	}
}

func valueAfter(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func TestYTDLPHelperProcess(t *testing.T) {
	mode := os.Getenv("READERGO_YTDLP_HELPER")
	if mode == "" {
		return
	}
	args := os.Args[1:]
	if argsPath := os.Getenv("READERGO_YTDLP_ARGS"); argsPath != "" {
		encoded, _ := json.Marshal(args)
		_ = os.WriteFile(argsPath, encoded, 0o600)
	}
	switch mode {
	case "success", "oversize":
		output := valueAfter(args, "--output")
		output = strings.ReplaceAll(output, "%(ext)s", "mp4")
		content := []byte("yt-dlp-media")
		if mode == "oversize" {
			content = []byte("oversized-media")
		}
		if err := os.WriteFile(output, content, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if mode == "oversize" {
			time.Sleep(10 * time.Second)
		}
		os.Exit(0)
	case "wait":
		_ = os.WriteFile(os.Getenv("READERGO_YTDLP_MARKER"), []byte("started"), 0o600)
		time.Sleep(10 * time.Second)
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}
