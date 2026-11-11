package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

type vector struct {
	Name         string   `json:"name"`
	Size         int64    `json:"size"`
	FileMD5      string   `json:"file_md5"`
	FileSHA      string   `json:"file_sha"`
	CheckSHA     string   `json:"check_sha"`
	CheckData    string   `json:"check_data"`
	BlockSHAList []string `json:"block_sha_list"`
}

func TestCalculateMatchesPythonReferenceVectors(t *testing.T) {
	fixture, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []vector
	if err := json.Unmarshal(fixture, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, want := range vectors {
		t.Run(want.Name, func(t *testing.T) {
			var file io.ReaderAt = patternReaderAt{size: want.Size}
			if want.Name == "abc" {
				file = bytes.NewReader([]byte("abc"))
			}
			got, err := Calculate(context.Background(), file, want.Size)
			if err != nil {
				t.Fatal(err)
			}
			if got.FileMD5 != want.FileMD5 || got.FileSHA != want.FileSHA || got.CheckSHA != want.CheckSHA || got.CheckData != want.CheckData || !reflect.DeepEqual(got.BlockSHAList, want.BlockSHAList) {
				t.Fatalf("Calculate() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCalculateRejectsUnsupportedAndInvalidInputs(t *testing.T) {
	if _, err := Calculate(context.Background(), bytes.NewReader(nil), 0); !errors.Is(err, ErrEmptyFileUnsupported) {
		t.Fatalf("zero size error = %v", err)
	}
	if _, err := Calculate(context.Background(), bytes.NewReader(nil), -1); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("negative size error = %v", err)
	}
	if _, err := Calculate(context.Background(), bytes.NewReader([]byte{1}), 2); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short reader error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Calculate(ctx, patternReaderAt{size: 1}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}

func TestCalculateReadsInBoundedBuffers(t *testing.T) {
	reader := &trackingReaderAt{reader: patternReaderAt{size: 2*blockSize + 1}}
	if _, err := Calculate(context.Background(), reader, 2*blockSize+1); err != nil {
		t.Fatal(err)
	}
	if reader.maxRead > hashReadBufferSize {
		t.Fatalf("largest ReadAt = %d, want <= %d", reader.maxRead, hashReadBufferSize)
	}
}

func TestUploadUsesSingleBase64LayerAndRepreflightsStateThree(t *testing.T) {
	fileData := []byte{0x00, 0xff, 0x80, 0x01, 0x02, 0x03}
	var calls atomic.Int32
	client, closeServer := testClient(t, func(w http.ResponseWriter, arguments map[string]json.RawMessage) {
		call := calls.Add(1)
		switch call {
		case 1:
			assertAbsent(t, arguments, "upload_key", "file_data")
			writeToolResponse(t, w, weiyun.UploadResponse{
				UploadKey: "uk+/=", Ex: "ex+/=",
				ChannelList: []weiyun.ChannelInfo{{ID: 7, Offset: 0, Len: 4}},
				UploadState: 1,
			})
		case 2:
			if got := string(arguments["file_data"]); got != `"AP+AAQ=="` {
				t.Errorf("file_data wire = %s", got)
			}
			if got := string(arguments["upload_key"]); got != `"uk+/="` {
				t.Errorf("upload_key = %s", got)
			}
			if got := string(arguments["ex"]); got != `"ex+/="` {
				t.Errorf("ex = %s", got)
			}
			if got := string(arguments["block_sha_list"]); got != `[]` {
				t.Errorf("block_sha_list = %s", got)
			}
			writeToolResponse(t, w, weiyun.UploadResponse{UploadState: 1})
		case 3:
			assertAbsent(t, arguments, "upload_key", "file_data")
			writeToolResponse(t, w, weiyun.UploadResponse{
				UploadKey: "wait-key", Ex: "wait-ex", UploadState: 3,
				ChannelList: []weiyun.ChannelInfo{{ID: 8, Offset: 4, Len: 0}},
			})
		case 4:
			assertAbsent(t, arguments, "upload_key", "file_data")
			writeToolResponse(t, w, weiyun.UploadResponse{
				UploadKey: "next-key", Ex: "next-ex",
				ChannelList: []weiyun.ChannelInfo{{ID: 9, Offset: 4, Len: 2}},
				UploadState: 1,
			})
		case 5:
			if got := string(arguments["file_data"]); got != `"AgM="` {
				t.Errorf("second file_data wire = %s", got)
			}
			if got := string(arguments["upload_key"]); got != `"next-key"` {
				t.Errorf("second upload_key = %s", got)
			}
			if got := string(arguments["ex"]); got != `"next-ex"` {
				t.Errorf("second ex = %s", got)
			}
			writeToolResponse(t, w, weiyun.UploadResponse{UploadState: 2, FileID: "file-id", Filename: "stored.bin"})
		default:
			t.Fatalf("unexpected call %d", call)
		}
	})
	defer closeServer()

	response, err := New(client).Upload(context.Background(), bytes.NewReader(fileData), int64(len(fileData)), "binary.bin", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if response.FileID != "file-id" || response.Filename != "stored.bin" || calls.Load() != 5 {
		t.Fatalf("response = %+v, calls = %d", response, calls.Load())
	}
}

func TestUploadRepreflightsAfterTemporaryChunkFailure(t *testing.T) {
	var calls atomic.Int32
	client, closeServer := testClient(t, func(w http.ResponseWriter, arguments map[string]json.RawMessage) {
		switch calls.Add(1) {
		case 1:
			writeToolResponse(t, w, weiyun.UploadResponse{
				UploadKey: "key", Ex: "ex", UploadState: 1,
				ChannelList: []weiyun.ChannelInfo{{ID: 1, Offset: 0, Len: 3}},
			})
		case 2:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("sensitive upstream body"))
		case 3:
			assertAbsent(t, arguments, "upload_key", "file_data")
			writeToolResponse(t, w, weiyun.UploadResponse{FileExist: true, FileID: "instant", Filename: "a.bin"})
		default:
			t.Fatalf("unexpected call")
		}
	})
	defer closeServer()

	response, err := New(client).Upload(context.Background(), bytes.NewReader([]byte("abc")), 3, "a.bin", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if response.FileID != "instant" || calls.Load() != 3 {
		t.Fatalf("response = %+v, calls = %d", response, calls.Load())
	}
}

func TestUploadDoesNotRetryPermanentChunkError(t *testing.T) {
	var calls atomic.Int32
	client, closeServer := testClient(t, func(w http.ResponseWriter, _ map[string]json.RawMessage) {
		switch calls.Add(1) {
		case 1:
			writeToolResponse(t, w, weiyun.UploadResponse{
				UploadKey: "key", Ex: "ex", UploadState: 1,
				ChannelList: []weiyun.ChannelInfo{{ID: 1, Offset: 0, Len: 3}},
			})
		case 2:
			writeToolResponse(t, w, map[string]any{"error": "-20001 sensitive sha error"})
		default:
			t.Fatalf("unexpected retry")
		}
	})
	defer closeServer()

	_, err := New(client).Upload(context.Background(), bytes.NewReader([]byte("abc")), 3, "a.bin", "parent")
	if !errors.Is(err, weiyun.ErrTool) {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestUploadRejectsMaliciousChannelsBeforeReading(t *testing.T) {
	tests := []struct {
		name    string
		channel weiyun.ChannelInfo
	}{
		{"offset_after_file", weiyun.ChannelInfo{ID: 1, Offset: 7, Len: 1}},
		{"length_after_file", weiyun.ChannelInfo{ID: 1, Offset: 5, Len: 2}},
		{"chunk_too_large", weiyun.ChannelInfo{ID: 1, Offset: 0, Len: maxChunkSize + 1}},
		{"overflow_offset", weiyun.ChannelInfo{ID: 1, Offset: math.MaxUint64, Len: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client, closeServer := testClient(t, func(w http.ResponseWriter, _ map[string]json.RawMessage) {
				calls.Add(1)
				writeToolResponse(t, w, weiyun.UploadResponse{
					UploadKey: "key", Ex: "ex", UploadState: 1,
					ChannelList: []weiyun.ChannelInfo{test.channel},
				})
			})
			defer closeServer()

			_, err := New(client).Upload(context.Background(), bytes.NewReader([]byte("123456")), 6, "a.bin", "parent")
			if !errors.Is(err, ErrInvalidChannel) {
				t.Fatalf("error = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d", calls.Load())
			}
		})
	}
}

func TestUploadStopsAfterConsecutiveNoProgress(t *testing.T) {
	var calls atomic.Int32
	client, closeServer := testClient(t, func(w http.ResponseWriter, _ map[string]json.RawMessage) {
		calls.Add(1)
		writeToolResponse(t, w, weiyun.UploadResponse{UploadState: 3})
	})
	defer closeServer()

	_, err := New(client).Upload(context.Background(), bytes.NewReader([]byte("abc")), 3, "a.bin", "parent")
	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() > maxNoProgress+1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestUploadHonorsContextCancellationDuringMCPCall(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	client, err := weiyun.NewClient(server.URL, "token", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = New(client).Upload(ctx, bytes.NewReader([]byte("abc")), 3, "a.bin", "parent")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

type patternReaderAt struct {
	size int64
}

func (r patternReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := len(p)
	if remaining := r.size - off; int64(n) > remaining {
		n = int(remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = byte((off + int64(i)) % 251)
	}
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

type trackingReaderAt struct {
	reader  io.ReaderAt
	maxRead int
}

func (r *trackingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	return r.reader.ReadAt(p, off)
}

func testClient(t *testing.T, handler func(http.ResponseWriter, map[string]json.RawMessage)) (*weiyun.Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Arguments map[string]json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		handler(w, request.Params.Arguments)
	}))
	client, err := weiyun.NewClient(server.URL, "token", "", server.Client())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server.Close
}

func writeToolResponse(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	text, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
		},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Fatal(err)
	}
}

func assertAbsent(t *testing.T, arguments map[string]json.RawMessage, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if _, ok := arguments[field]; ok {
			t.Errorf("unexpected %s", field)
		}
	}
}
