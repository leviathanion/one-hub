package relay_util

import (
	"context"
	"net/http"
	"one-api/common/requester"
	"sync"
	"time"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

const HeartbeatStreamText = "::PING\n\n"
const HeartbeatJsonText = "\n"

// 心跳配置
type HeartbeatConfig struct {
	TimeoutSeconds  int // 心跳超时时间（秒）
	IntervalSeconds int // 心跳间隔（秒）
}

// Heartbeat 心跳处理器
type Heartbeat struct {
	isStream   bool
	config     HeartbeatConfig
	ctx        context.Context
	cancel     context.CancelFunc
	writeMu    sync.Mutex
	headerSent bool
	c          *gin.Context
}

// NewHeartbeat 创建一个新的心跳处理器
func NewHeartbeat(isStream bool, config HeartbeatConfig, c *gin.Context) *Heartbeat {
	if config.IntervalSeconds < 5 {
		config.IntervalSeconds = 5 // 最低只能设置为5秒
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	return &Heartbeat{
		isStream: isStream,
		config:   config,
		ctx:      ctx,
		cancel:   cancel,
		c:        c,
	}
}

// HasWrittenHeader 检查头部是否已写入
func (h *Heartbeat) HasWrittenHeader() bool {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.headerSent
}

// writeHeader 立即写入头部，确保只写入一次
// 返回是否成功写入头部
func (h *Heartbeat) writeHeader() bool {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	if h.ctx.Err() != nil || h.headerSent {
		return false
	}
	h.headerSent = true

	// 写入头部
	if h.isStream {
		requester.SetEventStreamHeaders(h.c)
	} else {
		h.c.Writer.Header().Set("Content-Type", "application/json")
		h.c.Writer.WriteHeader(http.StatusOK)
	}
	return true
}

func (h *Heartbeat) writeHeartbeat() bool {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.ctx.Err() != nil {
		return false
	}

	message := HeartbeatJsonText
	if h.isStream {
		message = HeartbeatStreamText
	}
	if _, err := h.c.Writer.WriteString(message); err != nil {
		return false
	}
	h.c.Writer.Flush()
	return true
}

// Start 启动心跳机制，在指定的超时时间后开始发送心跳
func (h *Heartbeat) Start() {
	if h.ctx.Err() != nil {
		return
	}

	// 启动心跳协程
	gopool.Go(
		func() {
			// 等待超时时间后开始心跳
			timer := time.NewTimer(time.Duration(h.config.TimeoutSeconds) * time.Second)
			defer timer.Stop()

			select {
			case <-timer.C:
				// 写入头部
				if !h.writeHeader() {
					return
				}

				// 超时后开始发送心跳
				ticker := time.NewTicker(time.Duration(h.config.IntervalSeconds) * time.Second)
				defer ticker.Stop()

				for {
					select {
					case <-h.ctx.Done():
						// 心跳被取消
						return
					case <-ticker.C:
						if !h.writeHeartbeat() {
							h.Stop()
							return
						}
					}
				}
			case <-h.ctx.Done():
				return
			}
		})
}

// Close 关闭心跳（可以在defer中使用）
func (h *Heartbeat) Close() {
	h.Stop()
}

// Stop 等待已经开始的心跳写入结束，再移交响应 writer。
func (h *Heartbeat) Stop() {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	h.cancel()
}

// IsSafeWriteStream 检查是否可以安全地写入Stream
func (h *Heartbeat) IsSafeWriteStream() bool {
	h.Stop()

	if h.isStream {
		return h.HasWrittenHeader()
	}

	return false
}
