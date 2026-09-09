package alipay

type PayType string

const (
	FacePay        PayType = "facepay"
	PagePay        PayType = "pagepay"
	WapPay         PayType = "wappay"
	ProductFacePay         = "alipay.facepay"
	ProductPagePay         = "alipay.pagepay"
	ProductWapPay          = "alipay.wappay"
)
