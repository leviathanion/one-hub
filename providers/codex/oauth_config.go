package codex

// OAuth2 configuration shared by Codex authorization and token refresh flows.
const (
	DefaultClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	AuthorizeEndpoint  = "https://auth.openai.com/oauth/authorize"
	TokenEndpoint      = "https://auth.openai.com/oauth/token"
	DefaultRedirectURI = "http://localhost:1455/auth/callback"
	DefaultScope       = "openid profile email offline_access"
)
