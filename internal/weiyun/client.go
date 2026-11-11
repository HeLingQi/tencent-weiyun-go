package weiyun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	DefaultEndpoint = "https://www.weiyun.com/api/v3/mcpserver"
	DefaultQUA      = "LINUX_SERVER_TENCENTWEIYUNGO_0.1.0"
	ProtocolVersion = "1.0.3"

	maxResponseBytes   = 1 << 20
	defaultHTTPTimeout = 2 * time.Minute
)

var (
	ErrTransport        = errors.New("weiyun transport error")
	ErrHTTP             = errors.New("weiyun http error")
	ErrJSONRPC          = errors.New("weiyun json-rpc error")
	ErrTool             = errors.New("weiyun tool error")
	ErrInvalidResponse  = errors.New("invalid weiyun response")
	ErrResponseTooLarge = errors.New("weiyun response too large")
)

type Error struct {
	Kind       error
	HTTPStatus int
	Code       int
}

func (e *Error) Error() string {
	switch {
	case e.HTTPStatus != 0:
		return fmt.Sprintf("%s (status %d)", e.Kind, e.HTTPStatus)
	case e.Code != 0:
		return fmt.Sprintf("%s (code %d)", e.Kind, e.Code)
	default:
		return e.Kind.Error()
	}
}

func (e *Error) Unwrap() error {
	return e.Kind
}

func IsTemporary(err error) bool {
	var upstream *Error
	if !errors.As(err, &upstream) {
		return false
	}
	if errors.Is(upstream.Kind, ErrTransport) {
		return true
	}
	if errors.Is(upstream.Kind, ErrHTTP) {
		return upstream.HTTPStatus == http.StatusTooManyRequests || upstream.HTTPStatus >= 500
	}
	return upstream.Code == 117406
}

type Client struct {
	endpoint   *url.URL
	token      string
	cookie     string
	httpClient *http.Client
	requestID  atomic.Uint64
}

func NewClient(endpoint, token, cookie string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("invalid weiyun endpoint")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	clientCopy := *httpClient
	if clientCopy.Timeout <= 0 {
		clientCopy.Timeout = defaultHTTPTimeout
	}
	previousRedirect := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, u.Scheme) || !strings.EqualFold(req.URL.Host, u.Host) {
			return errors.New("weiyun redirect rejected")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("weiyun redirect limit exceeded")
		}
		return nil
	}
	return &Client{endpoint: u, token: token, cookie: cookie, httpClient: &clientCopy}, nil
}

func (c *Client) Call(ctx context.Context, tool string, arguments, output any) error {
	argumentJSON, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(argumentJSON, &fields); err != nil || fields == nil {
		return errors.New("weiyun arguments must be an object")
	}
	headerJSON, err := json.Marshal(ReqHeader{QUA: DefaultQUA, Version: ProtocolVersion})
	if err != nil {
		return err
	}
	fields["req_header"] = headerJSON

	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      c.requestID.Add(1),
		Method:  "tools/call",
		Params: struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}{Name: tool, Arguments: fields},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("WyHeader", "mcp_token="+c.token)
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Kind: ErrTransport}
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Kind: ErrTransport}
	}
	if len(responseBody) > maxResponseBytes {
		return &Error{Kind: ErrResponseTooLarge}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Kind: ErrHTTP, HTTPStatus: resp.StatusCode}
	}

	payload, err := jsonRPCPayload(responseBody, resp.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	return decodeToolResult(payload, output)
}

func jsonRPCPayload(body []byte, contentType string) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if json.Valid(trimmed) {
		return trimmed, nil
	}
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") && !bytes.HasPrefix(trimmed, []byte("data:")) && !bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil, &Error{Kind: ErrInvalidResponse}
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), maxResponseBytes)
	var dataLines []string
	var found []byte
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		candidate := []byte(strings.Join(dataLines, "\n"))
		if json.Valid(candidate) {
			found = append(found[:0], candidate...)
		}
		dataLines = dataLines[:0]
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if scanner.Err() != nil || len(found) == 0 {
		return nil, &Error{Kind: ErrInvalidResponse}
	}
	return found, nil
}

func decodeToolResult(payload []byte, output any) error {
	var response struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return &Error{Kind: ErrInvalidResponse}
	}
	if response.Error != nil {
		return &Error{Kind: ErrJSONRPC, Code: response.Error.Code}
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return &Error{Kind: ErrInvalidResponse}
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return &Error{Kind: ErrInvalidResponse}
	}
	if result.IsError {
		code := 0
		for _, item := range result.Content {
			code = parseErrorCode(item.Text)
			if code != 0 {
				break
			}
		}
		return &Error{Kind: ErrTool, Code: code}
	}

	toolPayload := result.StructuredContent
	if len(toolPayload) == 0 {
		for _, item := range result.Content {
			if item.Type == "text" && json.Valid([]byte(item.Text)) {
				toolPayload = json.RawMessage(item.Text)
				break
			}
		}
	}
	if len(toolPayload) == 0 {
		return &Error{Kind: ErrInvalidResponse}
	}

	var marker struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(toolPayload, &marker) == nil && hasToolError(marker.Error) {
		return &Error{Kind: ErrTool, Code: parseErrorCode(string(marker.Error))}
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(toolPayload, output); err != nil {
		return &Error{Kind: ErrInvalidResponse}
	}
	return nil
}

func hasToolError(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		return false
	}
	return true
}

func parseErrorCode(value string) int {
	for start := 0; start < len(value); start++ {
		if value[start] != '-' && (value[start] < '0' || value[start] > '9') {
			continue
		}
		end := start + 1
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if end == start+1 && value[start] == '-' {
			continue
		}
		code, err := strconv.Atoi(value[start:end])
		if err == nil {
			return code
		}
		start = end
	}
	return 0
}

func (c *Client) List(ctx context.Context, request ListRequest) (ListResponse, error) {
	var response ListResponse
	err := c.Call(ctx, "weiyun.list", request, &response)
	return response, err
}

func (c *Client) ListByCategory(ctx context.Context, request ListByCategoryRequest) (ListByCategoryResponse, error) {
	var response ListByCategoryResponse
	err := c.Call(ctx, "weiyun.list_by_category", request, &response)
	return response, err
}

func (c *Client) Download(ctx context.Context, request DownloadRequest) (DownloadResponse, error) {
	var response DownloadResponse
	err := c.Call(ctx, "weiyun.download", request, &response)
	return response, err
}

func (c *Client) Delete(ctx context.Context, request DeleteRequest) (DeleteResponse, error) {
	var response DeleteResponse
	err := c.Call(ctx, "weiyun.delete", request, &response)
	return response, err
}

func (c *Client) Upload(ctx context.Context, request UploadRequest) (UploadResponse, error) {
	var response UploadResponse
	err := c.Call(ctx, "weiyun.upload", request, &response)
	return response, err
}

func (c *Client) GenShareLink(ctx context.Context, request GenShareLinkRequest) (GenShareLinkResponse, error) {
	var response GenShareLinkResponse
	err := c.Call(ctx, "weiyun.gen_share_link", request, &response)
	return response, err
}

func (c *Client) RenameFile(ctx context.Context, request RenameFileRequest) (RenameFileResponse, error) {
	var response RenameFileResponse
	err := c.Call(ctx, "weiyun.rename_file", request, &response)
	return response, err
}

func (c *Client) RenameDir(ctx context.Context, request RenameDirRequest) (RenameDirResponse, error) {
	var response RenameDirResponse
	err := c.Call(ctx, "weiyun.rename_dir", request, &response)
	return response, err
}

func (c *Client) CreateDir(ctx context.Context, request CreateDirRequest) (CreateDirResponse, error) {
	var response CreateDirResponse
	err := c.Call(ctx, "weiyun.create_dir", request, &response)
	return response, err
}

func (c *Client) MoveDir(ctx context.Context, request MoveDirRequest) (MoveDirResponse, error) {
	var response MoveDirResponse
	err := c.Call(ctx, "weiyun.move_dir", request, &response)
	return response, err
}

func (c *Client) MoveFile(ctx context.Context, request MoveFileRequest) (MoveFileResponse, error) {
	var response MoveFileResponse
	err := c.Call(ctx, "weiyun.move_file", request, &response)
	return response, err
}
