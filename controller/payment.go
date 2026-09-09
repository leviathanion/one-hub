package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common"
	"one-api/model"
	paymentService "one-api/payment"
	"strconv"

	"github.com/gin-gonic/gin"
)

func GetPaymentList(c *gin.Context) {
	var params model.SearchPaymentParams
	if err := c.ShouldBindQuery(&params); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	payments, err := model.GetPanymentList(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	views := paymentViews(*payments.Data)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    model.DataResult[model.PaymentView]{Data: &views, Page: payments.Page, Size: payments.Size, TotalCount: payments.TotalCount},
	})
}

func GetPayment(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	payment, err := model.GetHistoricalPaymentByID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	var capabilities any
	products := []string{}
	if factory, factoryErr := paymentService.GetFactory(payment.Type); factoryErr == nil {
		products = factory.Descriptor().Products
	}
	if handle, acquireErr := paymentService.Resources.Acquire(c.Request.Context(), payment.Snapshot()); acquireErr == nil {
		if caps, capsErr := handle.Client.Capabilities(payment.DefaultProduct); capsErr == nil {
			capabilities = caps
		}
		handle.Release()
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": struct {
		model.PaymentView
		Capabilities any      `json:"capabilities"`
		Products     []string `json:"products"`
	}{payment.View(), capabilities, products}})
}

func AddPayment(c *gin.Context) {
	var p model.Payment
	if err := c.ShouldBindJSON(&p); err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	if err := paymentService.CreateGateway(c.Request.Context(), &p); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error(), "data": p.View()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "message": "", "data": p.View()})
}
func UpdatePayment(c *gin.Context) {
	var fields map[string]json.RawMessage
	if err := c.ShouldBindJSON(&fields); err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	var id int
	if err := json.Unmarshal(fields["id"], &id); err != nil || id <= 0 {
		common.APIRespondWithError(c, http.StatusBadRequest, errors.New("缺少支付网关 ID"))
		return
	}
	for _, key := range []string{"type", "uuid", "identity", "transaction_namespace", "protocol_profile", "credential_revision", "config", "setup_status", "setup_error"} {
		if _, exists := fields[key]; exists {
			common.APIRespondWithError(c, http.StatusBadRequest, errors.New("身份与凭证不能通过普通编辑修改，请新建网关或使用凭证轮换"))
			return
		}
	}
	p, err := model.GetPaymentByID(id)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	writable := map[string]any{"name": &p.Name, "icon": &p.Icon, "notify_domain": &p.NotifyDomain, "fixed_fee": &p.FixedFee, "percent_fee": &p.PercentFee, "currency": &p.Currency, "sort": &p.Sort, "enable": &p.Enable, "default_product": &p.DefaultProduct}
	for key, target := range writable {
		if raw, ok := fields[key]; ok {
			if string(raw) == "null" {
				common.APIRespondWithError(c, http.StatusBadRequest, errors.New("支付业务字段不能为 null"))
				return
			}
			if err := json.Unmarshal(raw, target); err != nil {
				common.APIRespondWithError(c, http.StatusBadRequest, err)
				return
			}
		}
	}
	if err := paymentService.UpdateGateway(c.Request.Context(), p); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": ""})
}
func RotatePaymentCredentials(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	var req struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Config           string `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	p, err := paymentService.RotateCredentials(c.Request.Context(), id, req.ExpectedRevision, req.Config)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": p.View()})
}

func DeletePayment(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	payment := model.Payment{ID: id}
	err = payment.Delete()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func GetUserPaymentList(c *gin.Context) {
	payments, err := model.GetUserPaymentList()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    paymentViews(payments),
	})
}

func paymentViews(payments []*model.Payment) []*model.PaymentView {
	if payments == nil {
		return nil
	}
	views := make([]*model.PaymentView, 0, len(payments))
	for _, p := range payments {
		view := p.View()
		views = append(views, &view)
	}
	return views
}
