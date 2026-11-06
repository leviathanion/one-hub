package requester

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"one-api/common/providerresponse"
)

const requestCompressionMinBytes = 1024

var requestZstdEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
})

type zstdRequestBody []byte

// WithZstdBody 只编码最终请求字节；小请求或压缩后更大的请求保持原样。
func (r *HTTPRequester) WithZstdBody(body []byte) requestOption {
	return r.WithBody(zstdRequestBody(body))
}

func encodeZstdRequestBody(body []byte) ([]byte, string, error) {
	if len(body) < requestCompressionMinBytes {
		return body, "", nil
	}
	encoder, err := requestZstdEncoder()
	if err != nil {
		return nil, "", err
	}
	compressed := encoder.EncodeAll(body, nil)
	if len(compressed) >= len(body) {
		return body, "", nil
	}
	return compressed, "zstd", nil
}

// ForResponseCompression 返回独立的传输视图，在协议解析和错误映射前解压。
// 原始 requester 仍保留 exact-wire 交付行为。
func (r *HTTPRequester) ForResponseCompression() *HTTPRequester {
	if r == nil {
		return nil
	}
	cloned := *r
	cloned.responseCompression = true
	return &cloned
}

func decodeCompressedResponse(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ",")))
	if encoding == "" || encoding == "identity" {
		return
	}
	resp.Body = &compressedResponseBody{source: resp.Body, encoding: encoding}
	providerresponse.StripRewrittenBodyHeaders(resp.Header)
	resp.ContentLength = -1
	resp.Uncompressed = true
}

// 延迟初始化让解压失败仍属于响应读取失败，保留已收到的状态与执行事实。
// 字节总量由既有 JSON/SSE 消费者限制；这里只限制 zstd 的解码窗口。
type compressedResponseBody struct {
	source   io.ReadCloser
	encoding string
	mu       sync.Mutex
	decoder  io.ReadCloser
	err      error
	close    sync.Once
	closeErr error
}

func (b *compressedResponseBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	if b.decoder == nil {
		switch b.encoding {
		case "gzip":
			var decoder *gzip.Reader
			decoder, b.err = gzip.NewReader(b.source)
			if b.err == nil {
				b.decoder = decoder
			}
		case "zstd":
			var decoder *zstd.Decoder
			decoder, b.err = zstd.NewReader(b.source,
				zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
				zstd.WithDecoderMaxWindow(128<<20), zstd.WithDecoderMaxMemory(128<<20))
			if b.err == nil {
				b.decoder = decoder.IOReadCloser()
			}
		default:
			b.err = fmt.Errorf("unsupported response content encoding %q", b.encoding)
		}
		if b.err != nil {
			return 0, b.err
		}
	}
	// 保留与最后一批字节同时返回的错误，避免 JSON decoder 提前成功后丢失完整性失败。
	n, err := b.decoder.Read(p)
	b.err = err
	return n, err
}

func (b *compressedResponseBody) Close() error {
	b.close.Do(func() {
		// 先关闭网络 body，解除可能持有 mu 的阻塞读取，再释放 decoder。
		b.closeErr = b.source.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.decoder != nil {
			_ = b.decoder.Close()
		}
		b.err = http.ErrBodyReadAfterClose
	})
	return b.closeErr
}
