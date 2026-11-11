package weiyun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListSendsHeadersSnakeCaseAndFixedReportingHeader(t *testing.T) {
	var request struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("WyHeader"); got != "mcp_token=test-token" {
			t.Errorf("WyHeader = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "uid=test-user" {
			t.Errorf("Cookie = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"pdir_key\":\"parent\",\"total_file_count\":1,\"file_list\":[{\"file_id\":\"file-1\",\"filename\":\"a.txt\",\"file_size\":3}],\"finish_flag\":true}"}]}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", "uid=test-user", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.List(context.Background(), ListRequest{
		GetType: 2,
		Offset:  4,
		Limit:   20,
		OrderBy: 2,
		Asc:     true,
		DirKey:  "dir",
		PDirKey: "parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.JSONRPC != "2.0" || request.Method != "tools/call" || request.Params.Name != "weiyun.list" {
		t.Fatalf("unexpected request: %+v", request)
	}
	for field, want := range map[string]string{
		"get_type": "2", "offset": "4", "limit": "20", "order_by": "2",
		"asc": "true", "dir_key": `"dir"`, "pdir_key": `"parent"`,
	} {
		if got := string(request.Params.Arguments[field]); got != want {
			t.Errorf("arguments[%q] = %s, want %s", field, got, want)
		}
	}
	var header ReqHeader
	if err := json.Unmarshal(request.Params.Arguments["req_header"], &header); err != nil {
		t.Fatal(err)
	}
	if header.QUA != DefaultQUA || header.Version != ProtocolVersion {
		t.Fatalf("req_header = %+v", header)
	}
	if response.PDirKey != "parent" || len(response.FileList) != 1 || response.FileList[0].FileID != "file-1" || !response.FinishFlag {
		t.Fatalf("response = %+v", response)
	}
}

func TestClientParsesSSEJSONRPCResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message\n")
		_, _ = fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"dir_key\":\"new-dir\",\"dir_name\":\"actual\"}"}]}}`)
		_, _ = fmt.Fprint(w, "\n\n")
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "token", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateDir(context.Background(), CreateDirRequest{PDirKey: "parent", DirName: "wanted"})
	if err != nil {
		t.Fatal(err)
	}
	if response.DirKey != "new-dir" || response.DirName != "actual" {
		t.Fatalf("response = %+v", response)
	}
}

func TestTypedOperationsCallAllWeiyunTools(t *testing.T) {
	requests := make(chan struct {
		Name      string
		Arguments map[string]json.RawMessage
	}, 11)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Name      string                     `json:"name"`
				Arguments map[string]json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests <- struct {
			Name      string
			Arguments map[string]json.RawMessage
		}{request.Params.Name, request.Params.Arguments}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "token", "cookie=value", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		name string
		call func() error
	}{
		{"weiyun.list", func() error { _, err := client.List(context.Background(), ListRequest{Limit: 1}); return err }},
		{"weiyun.list_by_category", func() error {
			_, err := client.ListByCategory(context.Background(), ListByCategoryRequest{Count: 1})
			return err
		}},
		{"weiyun.download", func() error { _, err := client.Download(context.Background(), DownloadRequest{}); return err }},
		{"weiyun.delete", func() error { _, err := client.Delete(context.Background(), DeleteRequest{}); return err }},
		{"weiyun.upload", func() error { _, err := client.Upload(context.Background(), UploadRequest{Filename: "a"}); return err }},
		{"weiyun.gen_share_link", func() error { _, err := client.GenShareLink(context.Background(), GenShareLinkRequest{}); return err }},
		{"weiyun.rename_file", func() error { _, err := client.RenameFile(context.Background(), RenameFileRequest{}); return err }},
		{"weiyun.rename_dir", func() error { _, err := client.RenameDir(context.Background(), RenameDirRequest{}); return err }},
		{"weiyun.create_dir", func() error { _, err := client.CreateDir(context.Background(), CreateDirRequest{}); return err }},
		{"weiyun.move_dir", func() error { _, err := client.MoveDir(context.Background(), MoveDirRequest{}); return err }},
		{"weiyun.move_file", func() error { _, err := client.MoveFile(context.Background(), MoveFileRequest{}); return err }},
	}
	for _, test := range calls {
		if err := test.call(); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		request := <-requests
		if request.Name != test.name {
			t.Fatalf("tool = %q, want %q", request.Name, test.name)
		}
		if _, ok := request.Arguments["req_header"]; !ok {
			t.Fatalf("%s missing req_header", test.name)
		}
	}
}

func TestClientRejectsCrossHostRedirectWithoutLeakingCredentials(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	client, err := NewClient(redirect.URL, "secret-token", "secret-cookie", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.List(context.Background(), ListRequest{Limit: 1})
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("error = %v", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests.Load())
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("error leaked sensitive data: %v", err)
	}
}

func TestClientBoundsResponseAndRedactsUpstreamErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want error
	}{
		{"http", "secret-token raw body", http.StatusBadGateway, ErrHTTP},
		{"jsonrpc", `{"jsonrpc":"2.0","id":1,"error":{"code":117402,"message":"secret-token"}}`, http.StatusOK, ErrJSONRPC},
		{"is_error", `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"secret-token"}]}}`, http.StatusOK, ErrTool},
		{"tool_error", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"error\":\"117406 secret-token\"}"}]}}`, http.StatusOK, ErrTool},
		{"invalid", `not-json secret-token`, http.StatusOK, ErrInvalidResponse},
		{"too_large", strings.Repeat("x", maxResponseBytes+1), http.StatusOK, ErrResponseTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "secret-token", "secret-cookie", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.List(context.Background(), ListRequest{Limit: 1})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "raw body") {
				t.Fatalf("error leaked upstream content: %v", err)
			}
		})
	}
}

func TestClientUsesFiniteDefaultTimeoutAndHonorsCancellation(t *testing.T) {
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

	client, err := NewClient(server.URL, "token", "", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != defaultHTTPTimeout || client.httpClient.Timeout <= 0 {
		t.Fatalf("timeout = %v", client.httpClient.Timeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.List(ctx, ListRequest{Limit: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewClientRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "relative", "ftp://example.com/mcp"} {
		if _, err := NewClient(endpoint, "token", "", nil); err == nil {
			t.Fatalf("NewClient(%q) succeeded", endpoint)
		}
	}
}
