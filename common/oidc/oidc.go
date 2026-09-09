package oidc

import (
	"context"
	"errors"
	"one-api/common/config"
	"one-api/common/logger"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type OIDCConfig struct {
	Provider     *oidc.Provider
	OAuth2Config *oauth2.Config
	Verifier     *oidc.IDTokenVerifier
	LoginURL     func(state string) string
}

type oidcConfigKey struct {
	enabled       bool
	issuer        string
	clientID      string
	clientSecret  string
	serverAddress string
	scopes        string
}

var oidcConfigCache struct {
	sync.Mutex
	key      oidcConfigKey
	instance *OIDCConfig
}

const oidcDiscoveryTimeout = 5 * time.Second

// 初始化OIDC配置
func InitOIDCConfig() error {
	ctx, cancel := context.WithTimeout(context.Background(), oidcDiscoveryTimeout)
	defer cancel()
	options := config.GlobalOption.RuntimeSnapshot()
	if !options.Bool("OIDCAuthEnabled", config.OIDCAuthEnabled) {
		return nil
	}
	_, err := GetOIDCConfigInstanceWithContext(ctx)
	return err
}

func buildOIDCConfig(ctx context.Context, key oidcConfigKey) (*OIDCConfig, error) {
	logger.SysLog("OIDC功能启用")
	discoveryCtx, cancel := context.WithTimeout(ctx, oidcDiscoveryTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(discoveryCtx, key.issuer)
	if err != nil {
		logger.SysError("OIDC配置错误, err:" + err.Error())
		return nil, err
	}

	oauth2Config := &oauth2.Config{
		ClientID:     key.clientID,
		ClientSecret: key.clientSecret,
		RedirectURL:  key.serverAddress + "/oauth/oidc",
		Endpoint:     provider.Endpoint(),
		Scopes:       strings.Split(key.scopes, ","),
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: oauth2Config.ClientID})

	instance := &OIDCConfig{
		Provider:     provider,
		OAuth2Config: oauth2Config,
		Verifier:     verifier,
		LoginURL: func(state string) string {
			return oauth2Config.AuthCodeURL(state, oauth2.AccessTypeOffline)
		},
	}
	return instance, nil
}

// 获取 OIDCConfig 实例，如果未初始化则进行初始化
func GetOIDCConfigInstance() (*OIDCConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), oidcDiscoveryTimeout)
	defer cancel()
	return GetOIDCConfigInstanceWithContext(ctx)
}

func GetOIDCConfigInstanceWithContext(ctx context.Context) (*OIDCConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key := currentOIDCConfigKey()
	if !key.enabled {
		return nil, errors.New("OIDC is disabled")
	}
	oidcConfigCache.Lock()
	if oidcConfigCache.instance != nil && oidcConfigCache.key == key {
		instance := oidcConfigCache.instance
		oidcConfigCache.Unlock()
		return instance, nil
	}
	oidcConfigCache.Unlock()

	instance, err := buildOIDCConfig(ctx, key)
	if err != nil {
		return nil, err
	}
	oidcConfigCache.Lock()
	if oidcConfigCache.instance != nil && oidcConfigCache.key == key {
		result := oidcConfigCache.instance
		oidcConfigCache.Unlock()
		return result, nil
	}
	if currentOIDCConfigKey() == key {
		oidcConfigCache.key = key
		oidcConfigCache.instance = instance
	}
	oidcConfigCache.Unlock()
	return instance, nil
}

func currentOIDCConfigKey() oidcConfigKey {
	options := config.GlobalOption.RuntimeSnapshot()
	return oidcConfigKey{
		enabled:       options.Bool("OIDCAuthEnabled", config.OIDCAuthEnabled),
		issuer:        options.String("OIDCIssuer", config.OIDCIssuer),
		clientID:      options.String("OIDCClientId", config.OIDCClientId),
		clientSecret:  options.String("OIDCClientSecret", config.OIDCClientSecret),
		serverAddress: options.String("ServerAddress", config.ServerAddress),
		scopes:        options.String("OIDCScopes", config.OIDCScopes),
	}
}
