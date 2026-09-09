package stripe

type StripeConfig struct {
	SecretKey         string `json:"secret_key"`
	WebhookSecret     string `json:"webhook_secret"`
	AccountID         string `json:"account_id"`
	Environment       string `json:"environment"`
	WebhookEndpointID string `json:"webhook_endpoint_id,omitempty"`
}
