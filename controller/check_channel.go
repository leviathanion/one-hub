package controller

import (
	"context"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/requester"
	"one-api/controller/check_channel"
	"time"

	"github.com/gin-gonic/gin"
)

func CheckImg(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	err := check_channel.AppendAccessRecord(id, c)
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	check_channel.CheckImageResponse(c)
}

type checkChannelRequest struct {
	ID     int    `json:"id"`
	Models string `json:"models"`
}

func CheckChannel(c *gin.Context) {
	var params checkChannelRequest
	if err := c.ShouldBindJSON(&params); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	ck, err := check_channel.CreateCheckChannel(ctx, params.ID, params.Models)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	// 设置 SSE 头信息
	requester.SetEventStreamHeaders(c)

	resultChan := make(chan *check_channel.ModelResult)
	done := make(chan struct{})
	go func() { defer close(done); ck.RunStream(ctx, resultChan) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// 处理结果流
	clientGone := c.Request.Context().Done()
	c.Stream(func(w io.Writer) bool {
		select {
		case result, ok := <-resultChan:
			if !ok {
				c.SSEvent("message", gin.H{"type": "done", "data": "completed"})
				return false
			}
			c.SSEvent("message", gin.H{
				"type": "result",
				"data": result,
			})
			return true
		case <-ticker.C:
			c.SSEvent("message", gin.H{
				"type": "heartbeat",
				"data": "ping",
			})
			return true
		case <-clientGone:
			return false
		}
	})
}
