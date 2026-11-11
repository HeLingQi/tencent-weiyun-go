package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HeLingQi/tencent-weiyun-go/internal/upload"
	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

func TestPutFileUsesHTTPBackendWithoutRealUpstream(t *testing.T) {
	uploaded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		switch request.Params.Name {
		case "weiyun.list":
			item := weiyun.FileItem{FileID: "old", Filename: "a.txt", FileSize: 3, FileMTime: 1}
			if uploaded {
				item = weiyun.FileItem{FileID: "new", Filename: "a.txt", FileSize: 4, FileMTime: 2}
			}
			writeRemoteToolResponse(t, w, weiyun.ListResponse{PDirKey: "root", FileList: []weiyun.FileItem{item}, FinishFlag: true})
		case "weiyun.upload":
			uploaded = true
			writeRemoteToolResponse(t, w, weiyun.UploadResponse{FileExist: true, FileID: "new", Filename: "a.txt"})
		default:
			t.Fatalf("unexpected tool %q", request.Params.Name)
		}
	}))
	defer server.Close()
	client, err := weiyun.NewClient(server.URL, "test-token", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := New(client, upload.New(client), nil)
	got, err := service.PutFile(context.Background(), "wy:/a.txt", &Version{FileID: "old", ParentKey: "root", Name: "a.txt", Size: 3, MTime: 1}, bytes.NewReader([]byte("data")), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !uploaded || got.FileID != "new" || got.Size != 4 || got.MTime != 2 {
		t.Fatalf("uploaded=%v version=%+v", uploaded, got)
	}
}

func writeRemoteToolResponse(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	text, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}}}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Fatal(err)
	}
}
