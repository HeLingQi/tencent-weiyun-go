package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

func TestPutFilePreflightsExistingVersionAndResolvesNewBaseline(t *testing.T) {
	current := weiyun.FileItem{FileID: "old-id", Filename: "a.txt", FileSize: 3, FileMTime: 10}
	client := &fakeClient{list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
		return weiyun.ListResponse{PDirKey: "root", FileList: []weiyun.FileItem{current}, FinishFlag: true}, nil
	}}
	uploader := &mountUploader{upload: func(_ context.Context, _ io.ReaderAt, size int64, name, parent string) (weiyun.UploadResponse, error) {
		if size != 4 || name != "a.txt" || parent != "root" {
			t.Fatalf("upload size=%d name=%q parent=%q", size, name, parent)
		}
		current = weiyun.FileItem{FileID: "new-id", Filename: name, FileSize: size, FileMTime: 20}
		return weiyun.UploadResponse{FileID: "new-id", Filename: name}, nil
	}}
	service := New(client, uploader, nil)

	got, err := service.PutFile(context.Background(), "wy:/a.txt", &Version{
		FileID: "old-id", ParentKey: "root", Name: "a.txt", Size: 3, MTime: 10,
	}, bytes.NewReader([]byte("data")), 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Version{FileID: "new-id", ParentKey: "root", Name: "a.txt", Size: 4, MTime: 20}) {
		t.Fatalf("version = %+v", got)
	}
	if uploader.calls != 1 {
		t.Fatalf("upload calls = %d", uploader.calls)
	}
}

func TestPutFileRejectsConflictBeforeUpload(t *testing.T) {
	current := weiyun.FileItem{FileID: "remote-id", Filename: "a.txt", FileSize: 5, FileMTime: 11}
	uploader := &mountUploader{}
	service := New(&fakeClient{list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
		return weiyun.ListResponse{PDirKey: "root", FileList: []weiyun.FileItem{current}, FinishFlag: true}, nil
	}}, uploader, nil)

	_, err := service.PutFile(context.Background(), "wy:/a.txt", &Version{
		FileID: "old-id", ParentKey: "root", Name: "a.txt", Size: 5, MTime: 11,
	}, bytes.NewReader([]byte("data")), 4)
	if !errors.Is(err, ErrVersionConflict) || uploader.calls != 0 {
		t.Fatalf("error=%v upload calls=%d", err, uploader.calls)
	}

	_, err = service.PutFile(context.Background(), "wy:/a.txt", nil, bytes.NewReader([]byte("data")), 4)
	if !errors.Is(err, ErrConflict) || uploader.calls != 0 {
		t.Fatalf("create error=%v upload calls=%d", err, uploader.calls)
	}
}

func TestPutFileRejectsResolvedSizeMismatchAfterUpload(t *testing.T) {
	current := weiyun.FileItem{FileID: "old", Filename: "a.txt", FileSize: 3, FileMTime: 1}
	client := &fakeClient{list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
		return weiyun.ListResponse{PDirKey: "root", FileList: []weiyun.FileItem{current}, FinishFlag: true}, nil
	}}
	uploader := &mountUploader{upload: func(context.Context, io.ReaderAt, int64, string, string) (weiyun.UploadResponse, error) {
		current = weiyun.FileItem{FileID: "new", Filename: "a.txt", FileSize: 2, FileMTime: 2}
		return weiyun.UploadResponse{FileID: "new", Filename: "a.txt"}, nil
	}}
	_, err := New(client, uploader, nil).PutFile(context.Background(), "wy:/a.txt", &Version{FileID: "old", ParentKey: "root", Name: "a.txt", Size: 3, MTime: 1}, bytes.NewReader([]byte("data")), 4)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("error=%v", err)
	}
}

func TestTrashNeverRequestsPermanentDelete(t *testing.T) {
	var request weiyun.DeleteRequest
	client := &fakeClient{
		list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
			return weiyun.ListResponse{PDirKey: "root", FileList: []weiyun.FileItem{{FileID: "f", Filename: "a"}}, FinishFlag: true}, nil
		},
		delete: func(_ context.Context, got weiyun.DeleteRequest) (weiyun.DeleteResponse, error) {
			request = got
			return weiyun.DeleteResponse{}, nil
		},
	}
	if err := New(client, nil, nil).Trash(context.Background(), "wy:/a"); err != nil {
		t.Fatal(err)
	}
	if request.DeleteCompletely || len(request.FileList) != 1 {
		t.Fatalf("request = %+v", request)
	}
}

func TestDirCacheCoalescesAndInvalidatesOnMutations(t *testing.T) {
	var listCalls int
	files := []weiyun.FileItem{
		{FileID: "f1", Filename: "a.txt", FileSize: 10, FileMTime: 1},
		{FileID: "f2", Filename: "b.txt", FileSize: 20, FileMTime: 2},
	}
	client := &fakeClient{
		list: func(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error) {
			listCalls++
			return weiyun.ListResponse{PDirKey: "root", FileList: append([]weiyun.FileItem(nil), files...), FinishFlag: true}, nil
		},
		createDir: func(_ context.Context, req weiyun.CreateDirRequest) (weiyun.CreateDirResponse, error) {
			return weiyun.CreateDirResponse{DirKey: "d1", DirName: req.DirName}, nil
		},
		delete: func(context.Context, weiyun.DeleteRequest) (weiyun.DeleteResponse, error) {
			files = files[1:]
			return weiyun.DeleteResponse{}, nil
		},
	}
	uploader := &mountUploader{upload: func(_ context.Context, _ io.ReaderAt, size int64, name, _ string) (weiyun.UploadResponse, error) {
		files = append(files, weiyun.FileItem{FileID: "f3", Filename: name, FileSize: size, FileMTime: 3})
		return weiyun.UploadResponse{FileID: "f3", Filename: name}, nil
	}}
	service := New(client, uploader, nil)
	service.SetCacheTTL(60_000_000_000, 60_000_000_000)

	if _, err := service.List(context.Background(), "wy:/"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := service.Resolve(context.Background(), "wy:/a.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Resolve(context.Background(), "wy:/b.txt"); err != nil {
			t.Fatal(err)
		}
	}
	if listCalls != 1 {
		t.Fatalf("expected 1 list call with dir cache, got %d", listCalls)
	}

	// PutFile must invalidate parent dir so Resolve sees the newly uploaded file immediately.
	if _, err := service.PutFile(context.Background(), "wy:/c.txt", nil, bytes.NewReader([]byte("xyz")), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), "wy:/c.txt"); err != nil {
		t.Fatal(err)
	}
	if listCalls != 2 {
		t.Fatalf("expected 2 list calls after PutFile + cached Resolve, got %d", listCalls)
	}

	// Trash must invalidate parent dir so deleted file disappears immediately.
	if err := service.Trash(context.Background(), "wy:/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), "wy:/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Trash, got %v", err)
	}
}

func TestReadRangeCachesURLRefreshesOn403AndValidatesContentRange(t *testing.T) {
	payload := []byte("0123456789abcdefABCDEF")
	var downloadCalls int
	currentToken := "expired"
	downloader := &Downloader{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cookie := req.Header.Get("Cookie")
			rangeHeader := req.Header.Get("Range")
			status, contentRange, data := func() (int, string, []byte) {
				if cookie != "FTN=valid" {
					return 403, "", nil
				}
				switch rangeHeader {
				case "bytes=0-3":
					return 206, "bytes 0-3/22", payload[0:4]
				case "bytes=10-15":
					return 206, "bytes 10-15/22", payload[10:16]
				case "bytes=4-7":
					return 206, "bytes 0-3/22", payload[0:4] // wrong range!
				default:
					return 416, "", nil
				}
			}()
			hdr := make(http.Header)
			if contentRange != "" {
				hdr.Set("Content-Range", contentRange)
			}
			return &http.Response{StatusCode: status, Header: hdr, Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
		})},
	}
	client := &fakeClient{
		download: func(context.Context, weiyun.DownloadRequest) (weiyun.DownloadResponse, error) {
			downloadCalls++
			if downloadCalls == 1 {
				currentToken = "expired"
			} else {
				currentToken = "valid"
			}
			return weiyun.DownloadResponse{Items: []weiyun.DownloadResultItem{{
				FileID: "f1", HTTPSDownloadURL: "https://cdn.example/f1", FileSize: int64(len(payload)), Cookie: "FTN=" + currentToken,
			}}}, nil
		},
	}
	service := New(client, nil, downloader)
	service.SetCacheTTL(60_000_000_000, 60_000_000_000)
	ver := Version{FileID: "f1", ParentKey: "root", Name: "f1.bin", Size: int64(len(payload)), MTime: 1}

	buf1 := make([]byte, 4)
	if err := service.ReadRange(context.Background(), ver, 0, buf1); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf1, []byte("0123")) || downloadCalls != 2 {
		t.Fatalf("buf1=%q downloadCalls=%d", buf1, downloadCalls)
	}

	buf2 := make([]byte, 6)
	if err := service.ReadRange(context.Background(), ver, 10, buf2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf2, []byte("abcdef")) || downloadCalls != 2 {
		t.Fatalf("buf2=%q downloadCalls=%d (expected cached URL)", buf2, downloadCalls)
	}

	bufBad := make([]byte, 4)
	if err := service.ReadRange(context.Background(), ver, 4, bufBad); err == nil {
		t.Fatal("expected error on mismatched Content-Range")
	}
}

type mountUploader struct {
	calls  int
	upload func(context.Context, io.ReaderAt, int64, string, string) (weiyun.UploadResponse, error)
}

func (u *mountUploader) Upload(ctx context.Context, file io.ReaderAt, size int64, name, parent string) (weiyun.UploadResponse, error) {
	u.calls++
	if u.upload == nil {
		return weiyun.UploadResponse{}, errors.New("unexpected upload")
	}
	return u.upload(ctx, file, size, name, parent)
}
