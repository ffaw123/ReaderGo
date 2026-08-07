package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"readergo/internal/media"
	"readergo/internal/model"
	"readergo/internal/queue"
)

func testPrepareOptions(maxSize int64) PrepareOptions {
	return PrepareOptions{
		MaxFileSize:           maxSize,
		MaxDuration:           6 * time.Hour,
		ConnectTimeout:        time.Second,
		ResponseHeaderTimeout: time.Second,
		DownloadTimeout:       5 * time.Second,
		MaxRedirects:          3,
		YTDLPPath:             "yt-dlp",
		YTDLPMaxHeight:        1080,
	}
}

type staticPrepareResolver struct {
	mu        sync.Mutex
	addresses map[string][]net.IPAddr
	lookups   []string
}

func (r *staticPrepareResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, host)
	return r.addresses[host], nil
}

func dialPrepareTestServer(address string) prepareDialContext {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

func TestPrepareDownloaderStreamsDirectURLAndFollowsValidatedRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redirect":
			http.Redirect(w, request, "http://cdn.example/video", http.StatusFound)
		case "/video":
			_, _ = w.Write([]byte("video-bytes"))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	resolver := &staticPrepareResolver{addresses: map[string][]net.IPAddr{
		"source.example": {{IP: net.ParseIP("93.184.216.34")}},
		"cdn.example":    {{IP: net.ParseIP("1.1.1.1")}},
	}}
	downloader, err := newPrepareDownloader(testPrepareOptions(1024), resolver, dialPrepareTestServer(server.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "video")
	size, err := downloader.Download(context.Background(), "http://source.example/redirect", destination)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) || string(content) != "video-bytes" {
		t.Fatalf("download = (%d, %q)", size, content)
	}
	directDestination := filepath.Join(t.TempDir(), "video")
	if _, err := downloader.Download(context.Background(), "http://source.example/video", directDestination); err != nil {
		t.Fatalf("direct download: %v", err)
	}
	if !containsString(resolver.lookups, "source.example") || !containsString(resolver.lookups, "cdn.example") {
		t.Fatalf("DNS lookups = %#v, want source and redirect hosts", resolver.lookups)
	}
}

func TestPrepareDownloaderRejectsSSRFOnInitialAndRedirectTargets(t *testing.T) {
	resolver := &staticPrepareResolver{addresses: map[string][]net.IPAddr{
		"source.example":   {{IP: net.ParseIP("93.184.216.34")}},
		"metadata.example": {{IP: net.ParseIP("169.254.169.254")}},
	}}
	if _, err := newPrepareDownloader(testPrepareOptions(1024), resolver, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial must not be reached")
	}); err != nil {
		t.Fatal(err)
	}
	direct, _ := newPrepareDownloader(testPrepareOptions(1024), resolver, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial must not be reached")
	})
	if _, err := direct.Download(context.Background(), "http://127.0.0.1/video", filepath.Join(t.TempDir(), "direct")); !isPrepareInputError(err) {
		t.Fatalf("loopback error = %v, want prepare input error", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, "http://metadata.example/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()
	redirect, err := newPrepareDownloader(testPrepareOptions(1024), resolver, dialPrepareTestServer(server.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = redirect.Download(context.Background(), "http://source.example/video", filepath.Join(t.TempDir(), "redirect"))
	if !isPrepareInputError(err) {
		t.Fatalf("redirect SSRF error = %v, want prepare input error", err)
	}
}

func TestPrepareDownloaderRejectsOversizedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		_, _ = w.Write([]byte("123456"))
	}))
	defer server.Close()
	resolver := &staticPrepareResolver{addresses: map[string][]net.IPAddr{
		"source.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	downloader, err := newPrepareDownloader(testPrepareOptions(5), resolver, dialPrepareTestServer(server.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloader.Download(context.Background(), "http://source.example/video", filepath.Join(t.TempDir(), "video"))
	if !isPrepareInputError(err) || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestPrepareDownloaderHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	resolver := &staticPrepareResolver{addresses: map[string][]net.IPAddr{
		"source.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	downloader, err := newPrepareDownloader(testPrepareOptions(1024), resolver, dialPrepareTestServer(server.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := downloader.Download(ctx, "http://source.example/video", filepath.Join(t.TempDir(), "video"))
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not stop after cancellation")
	}
}

func TestPrepareProcessorValidatesMediaUploadsStableObjectAndCleansTemp(t *testing.T) {
	repository := &fakePrepareRepository{video: model.Video{ID: "video-id", TenantID: "tenant-id", SourceURL: "https://source.example/video"}}
	objects := &fakePrepareObjectStore{}
	downloader := &fakePrepareDownloader{content: []byte("media")}
	processor := &PrepareProcessor{
		repository:  repository,
		prober:      fakePrepareProber{info: media.MediaInfo{DurationMS: 1200, FormatName: "mov,mp4", VideoCodec: "h264", Width: 1280, Height: 720}},
		objects:     objects,
		downloader:  downloader,
		maxSize:     1024,
		maxDuration: 6 * time.Hour,
	}
	err := processor.Process(context.Background(), queue.VideoJobMessage{TenantID: "tenant-id", VideoID: "video-id"})
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "tenants/tenant-id/videos/video-id/source/original"
	if objects.uploads != 1 || objects.uploadKey != wantKey || repository.savedKey != wantKey || repository.savedDuration != 1200 {
		t.Fatalf("upload/save = %#v / %#v", objects, repository)
	}
	if _, err := os.Stat(filepath.Dir(downloader.destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepare temporary directory still exists: %v", err)
	}
}

func TestPrepareProcessorTreatsNonMediaAsUserInputAndCleansTemp(t *testing.T) {
	downloader := &fakePrepareDownloader{content: []byte("not-media")}
	processor := &PrepareProcessor{
		repository:  &fakePrepareRepository{video: model.Video{ID: "video-id", TenantID: "tenant-id", SourceURL: "https://source.example/page"}},
		prober:      fakePrepareProber{err: errors.New("invalid data found")},
		objects:     &fakePrepareObjectStore{},
		downloader:  downloader,
		maxSize:     1024,
		maxDuration: 6 * time.Hour,
	}
	err := processor.Process(context.Background(), queue.VideoJobMessage{TenantID: "tenant-id", VideoID: "video-id"})
	if decideFailureAction(err, 1, 4) != failureActionFailWithoutDeadLetter {
		t.Fatalf("non-media error = %v, want user input failure", err)
	}
	if _, statErr := os.Stat(filepath.Dir(downloader.destination)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("prepare temporary directory still exists: %v", statErr)
	}
}

func TestPrepareProcessorCleansPartialFileAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	downloader := &cancelledPrepareDownloader{}
	processor := &PrepareProcessor{
		repository:  &fakePrepareRepository{video: model.Video{ID: "video-id", TenantID: "tenant-id", SourceURL: "https://source.example/video"}},
		prober:      fakePrepareProber{},
		objects:     &fakePrepareObjectStore{},
		downloader:  downloader,
		maxSize:     1024,
		maxDuration: 6 * time.Hour,
	}
	err := processor.Process(ctx, queue.VideoJobMessage{TenantID: "tenant-id", VideoID: "video-id"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(downloader.destination)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled prepare temporary directory still exists: %v", statErr)
	}
}

func TestPrepareProcessorReusesCompletedObject(t *testing.T) {
	key := "tenants/tenant-id/videos/video-id/source/original"
	downloader := &fakePrepareDownloader{content: []byte("must-not-download")}
	objects := &fakePrepareObjectStore{exists: true, size: 100}
	processor := &PrepareProcessor{
		repository:  &fakePrepareRepository{video: model.Video{ID: "video-id", TenantID: "tenant-id", ObjectKey: key, DurationMS: 1000}},
		prober:      fakePrepareProber{err: errors.New("must not probe")},
		objects:     objects,
		downloader:  downloader,
		maxSize:     1024,
		maxDuration: 6 * time.Hour,
	}
	if err := processor.Process(context.Background(), queue.VideoJobMessage{TenantID: "tenant-id", VideoID: "video-id"}); err != nil {
		t.Fatal(err)
	}
	if downloader.calls != 0 || objects.uploads != 0 || objects.downloads != 0 {
		t.Fatalf("idempotent retry performed work: downloader=%d store=%#v", downloader.calls, objects)
	}
}

func TestValidatePreparedMediaRejectsOverlongVideo(t *testing.T) {
	info := media.MediaInfo{DurationMS: (2 * time.Hour).Milliseconds(), FormatName: "mp4", VideoCodec: "h264", Width: 1280, Height: 720}
	if err := validatePreparedMedia(info, time.Hour); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("duration validation error = %v", err)
	}
}

func TestParsePrepareURLRejectsCredentialsUnsupportedSchemesAndPorts(t *testing.T) {
	for _, rawURL := range []string{"ftp://example.com/video", "https://user:pass@example.com/video", "https://example.com:70000/video"} {
		if _, err := parsePrepareURL(rawURL); !isPrepareInputError(err) {
			t.Errorf("parsePrepareURL(%q) error = %v", rawURL, err)
		}
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

type fakePrepareRepository struct {
	video         model.Video
	savedKey      string
	savedDuration int64
}

func (r *fakePrepareRepository) FindVideo(context.Context, string, string) (model.Video, error) {
	return r.video, nil
}

func (r *fakePrepareRepository) SavePreparedVideo(_ context.Context, _, _ string, objectKey string, durationMS int64) error {
	r.savedKey, r.savedDuration = objectKey, durationMS
	return nil
}

type fakePrepareProber struct {
	info media.MediaInfo
	err  error
}

func (p fakePrepareProber) Probe(context.Context, string) (media.MediaInfo, error) {
	return p.info, p.err
}

type fakePrepareObjectStore struct {
	exists    bool
	size      int64
	downloads int
	uploads   int
	uploadKey string
}

func (s *fakePrepareObjectStore) StatObject(context.Context, string) (int64, bool, error) {
	return s.size, s.exists, nil
}

func (s *fakePrepareObjectStore) DownloadObject(context.Context, string, string) error {
	s.downloads++
	return nil
}

func (s *fakePrepareObjectStore) UploadObject(_ context.Context, objectKey, _ string, _ int64) error {
	s.uploads++
	s.uploadKey = objectKey
	return nil
}

type fakePrepareDownloader struct {
	content     []byte
	destination string
	calls       int
}

func (d *fakePrepareDownloader) Download(_ context.Context, _ string, destination string) (int64, error) {
	d.calls++
	d.destination = destination
	if err := os.WriteFile(destination, d.content, 0o600); err != nil {
		return 0, err
	}
	return int64(len(d.content)), nil
}

type cancelledPrepareDownloader struct{ destination string }

func (d *cancelledPrepareDownloader) Download(ctx context.Context, _ string, destination string) (int64, error) {
	d.destination = destination
	if err := os.WriteFile(destination, []byte("partial"), 0o600); err != nil {
		return 0, err
	}
	return 0, ctx.Err()
}
