package wxpay

type WeChatConfig struct {
	AppID                      string `json:"app_id"`
	MchID                      string `json:"mch_id"`
	MchCertificateSerialNumber string `json:"mch_certificate_serial_number"`
	MchAPIv3Key                string `json:"mch_apiv3_key"`
	MchPrivateKey              string `json:"mch_private_key"`
	NotifyURL                  string `json:"notify_url,omitempty"`
	PayType                    string `json:"pay_type"`
}
