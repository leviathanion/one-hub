package epay

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"sort"
	"strings"
)

type Client struct {
	PayDomain string `json:"pay_domain"`
	PartnerID string `json:"partner_id"`
	Key       string `json:"key"`
}

// FormPay 表单支付
func (c *Client) FormPay(args *PayArgs) (string, map[string]string, error) {
	formPayArgs := map[string]string{
		"type":         string(args.Type),
		"pid":          c.PartnerID,
		"out_trade_no": args.OutTradeNo,
		"notify_url":   args.NotifyUrl,
		"return_url":   args.ReturnUrl,
		"name":         args.Name,
		"money":        args.Money,
		// "device":       string(args.Device),
	}

	formPayArgs["sign"] = c.Sign(formPayArgs)
	formPayArgs["sign_type"] = FormArgsSignType

	domain := strings.TrimSuffix(c.PayDomain, "/")

	return domain + FormSubmitUrl, formPayArgs, nil
}

func (c *Client) Verify(params map[string]string) (*PaymentResult, bool) {
	sign := params["sign"]
	if sign == "" || (params["sign_type"] != "" && params["sign_type"] != FormArgsSignType) {
		return nil, false
	}

	if subtle.ConstantTimeCompare([]byte(sign), []byte(c.Sign(params))) != 1 {
		return nil, false
	}

	return &PaymentResult{PartnerID: params["pid"], Type: PayType(params["type"]), TradeNo: params["trade_no"], OutTradeNo: params["out_trade_no"], Name: params["name"], Money: params["money"], TradeStatus: params["trade_status"]}, true
}

// Sign 签名
func (c *Client) Sign(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != "sign" && k != "sign_type" && args[k] != "" {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	signStrs := make([]string, len(keys))
	for i, k := range keys {
		signStrs[i] = k + "=" + args[k]
	}

	signStr := strings.Join(signStrs, "&") + c.Key

	h := md5.New()
	h.Write([]byte(signStr))

	return hex.EncodeToString(h.Sum(nil))
}
