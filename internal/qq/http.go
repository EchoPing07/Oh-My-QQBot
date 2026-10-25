package qq

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// newPutRequest 构造分片上传请求。
func newPutRequest(ctx context.Context, url string, data []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(data))
	return req, nil
}

// drainAndClose 读完并关闭响应体，让连接可以被复用。
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
}
