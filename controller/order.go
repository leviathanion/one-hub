package controller

import (
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	"one-api/payment"
	"one-api/payment/types"
)

type OrderRequest = payment.CreateOrderRequest

func CreateOrder(c *gin.Context) {
	var req OrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	view, err := payment.CreateOrder(c.Request.Context(), c.GetInt("id"), req)
	orderResponse(c, view, err)
}
func orderResponse(c *gin.Context, view *payment.OrderView, err error) {
	c.Header("Cache-Control", "no-store")
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": view})
}
func CheckOrderStatus(c *gin.Context) {
	view, err := payment.Status(c.Request.Context(), c.GetInt("id"), c.Query("trade_no"))
	orderResponse(c, view, err)
}
func QueryOrder(c *gin.Context) {
	var req struct {
		TradeNo string `json:"trade_no"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		orderResponse(c, nil, err)
		return
	}
	view, err := payment.QueryOrder(c.Request.Context(), c.GetInt("id"), req.TradeNo)
	orderResponse(c, view, err)
}
func AdminQueryOrder(c *gin.Context) {
	view, err := payment.QueryOrder(c.Request.Context(), 0, c.Param("trade_no"))
	orderResponse(c, view, err)
}
func CloseOrder(c *gin.Context) {
	var req struct {
		TradeNo string `json:"trade_no"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		orderResponse(c, nil, err)
		return
	}
	view, err := payment.CloseOrder(c.Request.Context(), c.GetInt("id"), req.TradeNo)
	orderResponse(c, view, err)
}
func PaymentCallback(c *gin.Context) {
	service, err := payment.NewHistoricalPaymentService(c.Param("uuid"))
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	response, err := service.Callback(c.Request.Context(), types.CallbackRequest{Method: c.Request.Method, Query: c.Request.URL.Query(), Headers: c.Request.Header.Clone(), Body: body})
	if err != nil {
		logger.SysError("支付通知处理失败: " + err.Error())
	}
	for key, values := range response.Headers {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	if response.StatusCode == 0 {
		response.StatusCode = http.StatusServiceUnavailable
	}
	c.Status(response.StatusCode)
	if len(response.Body) > 0 {
		_, _ = c.Writer.Write(response.Body)
	}
}

// 管理展示仍使用派生浮点字段；支付核对只使用冻结最小单位金额。
func calculateOrderAmount(p *model.Payment, amount int) (discount, fee, money float64) {
	quote, err := payment.CalculateQuote(p, amount)
	if err != nil {
		return
	}
	return quote.Discount, quote.Fee, float64(quote.Total.Minor) / 100
}

func GetOrderList(c *gin.Context) {
	var params model.SearchOrderParams
	if err := c.ShouldBindQuery(&params); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	payments, err := model.GetOrderList(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    payments,
	})
}
