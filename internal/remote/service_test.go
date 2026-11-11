package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

func TestNormalizeRejectsInvalidAndEscapingPaths(t *testing.T) {
	for input, want := range map[string]string{
		"wy:/": "/", "wy://a/./b": "/a/b", "wy:/a/../b": "/b",
	} {
		got, err := Normalize(input)
		if err != nil || got != want {
			t.Fatalf("Normalize(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"/a", "wy:", "wy:/../a", "wy:/a/../../b"} {
		if _, err := Normalize(input); err == nil {
			t.Fatalf("Normalize(%q) succeeded", input)
		}
	}
}

func TestResolveUsesPaginationAndTopLevelParentKey(t *testing.T) {
	var requests []weiyun.ListRequest
	client := &fakeClient{list: func(_ context.Context, request weiyun.ListRequest) (weiyun.ListResponse, error) {
		requests = append(requests, request)
		switch {
		case request.DirKey == "" && request.Offset == 0:
			return weiyun.ListResponse{PDirKey: "root-key", DirList: []weiyun.DirItem{{DirKey: "docs-key", DirName: "docs"}}}, nil
		case request.DirKey == "" && request.Offset == 1:
			return weiyun.ListResponse{PDirKey: "root-key", FinishFlag: true}, nil
		case request.DirKey == "docs-key":
			return weiyun.ListResponse{PDirKey: "docs-parent-key", FileList: []weiyun.FileItem{{FileID: "file-1", Filename: "a.txt", PDirKey: ""}}, FinishFlag: true}, nil
		default:
			return weiyun.ListResponse{}, errors.New("unexpected list request")
		}
	}}
	service := New(client, nil, nil)

	entry, err := service.Resolve(context.Background(), "wy:/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.File == nil || entry.File.FileID != "file-1" || entry.ParentKey != "docs-parent-key" {
		t.Fatalf("entry = %+v", entry)
	}
	if len(requests) != 3 || requests[1].Offset != 1 || requests[2].PDirKey != "root-key" {
		t.Fatalf("requests = %+v", requests)
	}
}

func TestResolveRejectsAmbiguityAndPaginationNoProgress(t *testing.T) {
	t.Run("ambiguity", func(t *testing.T) {
		client := &fakeClient{list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
			return weiyun.ListResponse{PDirKey: "root", DirList: []weiyun.DirItem{{DirKey: "d1", DirName: "same"}, {DirKey: "d2", DirName: "same"}}, FinishFlag: true}, nil
		}}
		_, err := New(client, nil, nil).Resolve(context.Background(), "wy:/same")
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no progress", func(t *testing.T) {
		client := &fakeClient{list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
			return weiyun.ListResponse{PDirKey: "root"}, nil
		}}
		_, err := New(client, nil, nil).List(context.Background(), "wy:/")
		if !errors.Is(err, ErrNoProgress) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestMoveMapsRenameAndCrossDirectoryMoveWithoutOverwrite(t *testing.T) {
	entries := map[string]Entry{
		"/a.txt":    {Path: "/a.txt", Name: "a.txt", ParentKey: "root", File: &weiyun.FileItem{FileID: "f1", Filename: "a.txt"}},
		"/folder":   {Path: "/folder", Name: "folder", ParentKey: "root", Dir: &weiyun.DirItem{DirKey: "folder-key", DirName: "folder"}},
		"/folder/x": {Path: "/folder/x", Name: "x", ParentKey: "folder-key", File: &weiyun.FileItem{FileID: "existing", Filename: "x"}},
		"/other":    {Path: "/other", Name: "other", ParentKey: "root", Dir: &weiyun.DirItem{DirKey: "other-key", DirName: "other"}},
	}
	var renamed *weiyun.RenameFileRequest
	var moved *weiyun.MoveFileRequest
	client := &fakeClient{
		list: listFromEntries(entries),
		renameFile: func(_ context.Context, request weiyun.RenameFileRequest) (weiyun.RenameFileResponse, error) {
			renamed = &request
			return weiyun.RenameFileResponse{}, nil
		},
		moveFile: func(_ context.Context, request weiyun.MoveFileRequest) (weiyun.MoveFileResponse, error) {
			moved = &request
			return weiyun.MoveFileResponse{}, nil
		},
	}
	service := New(client, nil, nil)

	if err := service.Move(context.Background(), "wy:/a.txt", "wy:/b.txt"); err != nil {
		t.Fatal(err)
	}
	if renamed == nil || renamed.FileID != "f1" || renamed.PDirKey != "root" || renamed.NewFilename != "b.txt" {
		t.Fatalf("rename request = %+v", renamed)
	}
	if err := service.Move(context.Background(), "wy:/a.txt", "wy:/other/a.txt"); err != nil {
		t.Fatal(err)
	}
	if moved == nil || moved.SrcPDirKey != "root" || moved.DstPDirKey != "other-key" || moved.Filename != "a.txt" {
		t.Fatalf("move request = %+v", moved)
	}
	if err := service.Move(context.Background(), "wy:/a.txt", "wy:/other/renamed.txt"); !errors.Is(err, ErrCrossDirectoryRename) {
		t.Fatalf("cross rename error = %v", err)
	}
	if err := service.Move(context.Background(), "wy:/a.txt", "wy:/folder/x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if err := service.Move(context.Background(), "wy:/", "wy:/other/root"); !errors.Is(err, ErrRootOperation) {
		t.Fatalf("root move error = %v", err)
	}
}

func TestDeleteUsesTopLevelParentKeyAndPermanentMapping(t *testing.T) {
	entries := map[string]Entry{
		"/a.txt": {Path: "/a.txt", Name: "a.txt", ParentKey: "root-key", File: &weiyun.FileItem{FileID: "f1", Filename: "a.txt", PDirKey: "wrong"}},
	}
	var requests []weiyun.DeleteRequest
	client := &fakeClient{
		list: listFromEntries(entries),
		delete: func(_ context.Context, request weiyun.DeleteRequest) (weiyun.DeleteResponse, error) {
			requests = append(requests, request)
			return weiyun.DeleteResponse{}, nil
		},
	}
	service := New(client, nil, nil)
	if err := service.Delete(context.Background(), "wy:/a.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), "wy:/a.txt", true); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].DeleteCompletely || !requests[1].DeleteCompletely || requests[0].FileList[0].PDirKey != "root" {
		t.Fatalf("delete requests = %+v", requests)
	}
	if err := service.Delete(context.Background(), "wy:/", false); !errors.Is(err, ErrRootOperation) {
		t.Fatalf("root delete error = %v", err)
	}
}

func TestSecureHTTPClientDisablesProxy(t *testing.T) {
	client := secureHTTPClient(func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("proxy is enabled")
	}
}

func TestDownloadStreamsAndPublishesWithoutOverwrite(t *testing.T) {
	body := []byte("streamed-data")
	roundTripper := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://download.example/file" || request.Header.Get("Cookie") != "FTN=x" {
			t.Fatalf("request = %s cookie=%q", request.URL, request.Header.Get("Cookie"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})
	downloader := &Downloader{
		HTTPClient: &http.Client{Transport: roundTripper},
		LookupIP:   func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
	}
	entries := map[string]Entry{
		"/a.txt": {Path: "/a.txt", Name: "a.txt", ParentKey: "root", File: &weiyun.FileItem{FileID: "f1", Filename: "a.txt"}},
	}
	client := &fakeClient{
		list: listFromEntries(entries),
		download: func(context.Context, weiyun.DownloadRequest) (weiyun.DownloadResponse, error) {
			return weiyun.DownloadResponse{Items: []weiyun.DownloadResultItem{{FileID: "f1", HTTPSDownloadURL: "https://download.example/file", FileSize: int64(len(body)), Cookie: "FTN=x"}}}, nil
		},
	}
	service := New(client, nil, downloader)
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	if err := service.Download(context.Background(), "wy:/a.txt", target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("downloaded = %q, %v", got, err)
	}
	if err := service.Download(context.Background(), "wy:/a.txt", target); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".out.txt.tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, %v", matches, err)
	}
}

func TestDownloaderRejectsUnsafeURLsAndCleansFailures(t *testing.T) {
	for _, rawURL := range []string{"http://example.com/file", "https://localhost/file", "https://127.0.0.1/file"} {
		t.Run(rawURL, func(t *testing.T) {
			downloader := &Downloader{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unsafe request reached transport")
				return nil, nil
			})}}
			err := downloader.Download(context.Background(), rawURL, "", 1, filepath.Join(t.TempDir(), "out"))
			if !errors.Is(err, ErrUnsafeDownload) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "out")
	downloader := &Downloader{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("secret upstream body")), Request: request}, nil
		})},
		LookupIP: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
	}
	err := downloader.Download(context.Background(), "https://download.example/file", "secret-cookie", 1, target)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".out.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary files = %v", matches)
	}
}

func TestDownloaderRejectsCrossHostRedirectBeforeCookieCanLeak(t *testing.T) {
	calls := 0
	downloader := &Downloader{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls > 1 {
				t.Fatalf("redirect reached %s with cookie %q", request.URL, request.Header.Get("Cookie"))
			}
			header := make(http.Header)
			header.Set("Location", "https://evil.example/file")
			return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		})},
		LookupIP: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
	}
	err := downloader.Download(context.Background(), "https://download.example/file", "secret-cookie", 0, filepath.Join(t.TempDir(), "out"))
	if !errors.Is(err, ErrUnsafeDownload) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestMkdirUploadAndShareRejectSilentRenameAndUseResolvedParent(t *testing.T) {
	entries := map[string]Entry{
		"/a.txt": {Path: "/a.txt", Name: "a.txt", ParentKey: "root", File: &weiyun.FileItem{FileID: "f1", Filename: "a.txt"}},
	}
	var createRequest weiyun.CreateDirRequest
	var shareRequest weiyun.GenShareLinkRequest
	client := &fakeClient{
		list: listFromEntries(entries),
		createDir: func(_ context.Context, request weiyun.CreateDirRequest) (weiyun.CreateDirResponse, error) {
			createRequest = request
			return weiyun.CreateDirResponse{DirKey: "new-key", DirName: request.DirName}, nil
		},
		share: func(_ context.Context, request weiyun.GenShareLinkRequest) (weiyun.GenShareLinkResponse, error) {
			shareRequest = request
			return weiyun.GenShareLinkResponse{ShortURL: "https://share.example/x"}, nil
		},
	}
	uploader := &fakeUploader{}
	service := New(client, uploader, nil)
	created, err := service.Mkdir(context.Background(), "wy:/new")
	if err != nil || created != "/new" || createRequest.PDirKey != "root" {
		t.Fatalf("created=%q request=%+v error=%v", created, createRequest, err)
	}
	local := filepath.Join(t.TempDir(), "local.bin")
	if err := os.WriteFile(local, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Upload(context.Background(), local, "wy:/stored.bin"); err != nil {
		t.Fatal(err)
	}
	if uploader.filename != "stored.bin" || uploader.parentKey != "root" || uploader.size != 3 {
		t.Fatalf("upload = %+v", uploader)
	}
	if err := service.Upload(context.Background(), local, "wy:/"); err != nil {
		t.Fatal(err)
	}
	if uploader.filename != "local.bin" || uploader.parentKey != "root" {
		t.Fatalf("root upload = %+v", uploader)
	}
	link, err := service.Share(context.Background(), "wy:/a.txt")
	if err != nil || link == "" || shareRequest.FileList[0].PDirKey != "root" {
		t.Fatalf("share=%q request=%+v error=%v", link, shareRequest, err)
	}

	client.createDir = func(_ context.Context, request weiyun.CreateDirRequest) (weiyun.CreateDirResponse, error) {
		return weiyun.CreateDirResponse{DirName: request.DirName + " (1)"}, nil
	}
	if _, err := service.Mkdir(context.Background(), "wy:/renamed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("silent rename error = %v", err)
	}
}

type fakeClient struct {
	list       func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error)
	download   func(context.Context, weiyun.DownloadRequest) (weiyun.DownloadResponse, error)
	delete     func(context.Context, weiyun.DeleteRequest) (weiyun.DeleteResponse, error)
	renameFile func(context.Context, weiyun.RenameFileRequest) (weiyun.RenameFileResponse, error)
	moveFile   func(context.Context, weiyun.MoveFileRequest) (weiyun.MoveFileResponse, error)
	createDir  func(context.Context, weiyun.CreateDirRequest) (weiyun.CreateDirResponse, error)
	share      func(context.Context, weiyun.GenShareLinkRequest) (weiyun.GenShareLinkResponse, error)
}

func (f *fakeClient) List(ctx context.Context, request weiyun.ListRequest) (weiyun.ListResponse, error) {
	return f.list(ctx, request)
}
func (f *fakeClient) Download(ctx context.Context, request weiyun.DownloadRequest) (weiyun.DownloadResponse, error) {
	return f.download(ctx, request)
}
func (f *fakeClient) Delete(ctx context.Context, request weiyun.DeleteRequest) (weiyun.DeleteResponse, error) {
	return f.delete(ctx, request)
}
func (f *fakeClient) RenameFile(ctx context.Context, request weiyun.RenameFileRequest) (weiyun.RenameFileResponse, error) {
	return f.renameFile(ctx, request)
}
func (f *fakeClient) RenameDir(context.Context, weiyun.RenameDirRequest) (weiyun.RenameDirResponse, error) {
	panic("unexpected RenameDir")
}
func (f *fakeClient) CreateDir(ctx context.Context, request weiyun.CreateDirRequest) (weiyun.CreateDirResponse, error) {
	return f.createDir(ctx, request)
}
func (f *fakeClient) MoveDir(context.Context, weiyun.MoveDirRequest) (weiyun.MoveDirResponse, error) {
	panic("unexpected MoveDir")
}
func (f *fakeClient) MoveFile(ctx context.Context, request weiyun.MoveFileRequest) (weiyun.MoveFileResponse, error) {
	return f.moveFile(ctx, request)
}
func (f *fakeClient) GenShareLink(ctx context.Context, request weiyun.GenShareLinkRequest) (weiyun.GenShareLinkResponse, error) {
	return f.share(ctx, request)
}

type fakeUploader struct {
	size      int64
	filename  string
	parentKey string
}

func (f *fakeUploader) Upload(_ context.Context, _ io.ReaderAt, size int64, filename, parentKey string) (weiyun.UploadResponse, error) {
	f.size, f.filename, f.parentKey = size, filename, parentKey
	return weiyun.UploadResponse{Filename: filename}, nil
}

func listFromEntries(entries map[string]Entry) func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
	keyToPath := map[string]string{"": "/", "folder-key": "/folder", "other-key": "/other"}
	return func(_ context.Context, request weiyun.ListRequest) (weiyun.ListResponse, error) {
		path := keyToPath[request.DirKey]
		parentKey := "root"
		if path != "/" {
			parentKey = request.DirKey
		}
		response := weiyun.ListResponse{PDirKey: parentKey, FinishFlag: true}
		for entryPath, entry := range entries {
			if filepath.ToSlash(filepath.Dir(entryPath)) != path {
				continue
			}
			if entry.Dir != nil {
				response.DirList = append(response.DirList, *entry.Dir)
			} else if entry.File != nil {
				response.FileList = append(response.FileList, *entry.File)
			}
		}
		return response, nil
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestListCombinesAllPages(t *testing.T) {
	client := &fakeClient{list: func(_ context.Context, request weiyun.ListRequest) (weiyun.ListResponse, error) {
		if request.Offset == 0 {
			return weiyun.ListResponse{PDirKey: "root", DirList: []weiyun.DirItem{{DirKey: "d", DirName: "dir"}}}, nil
		}
		return weiyun.ListResponse{PDirKey: "root", FileList: []weiyun.FileItem{{FileID: "f", Filename: "file"}}, FinishFlag: true}, nil
	}}
	listing, err := New(client, nil, nil).List(context.Background(), "wy:/")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]int{len(listing.Dirs), len(listing.Files)}, []int{1, 1}) {
		t.Fatalf("listing = %+v", listing)
	}
}
