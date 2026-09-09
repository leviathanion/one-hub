package model

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestRemotePriceCatalogRequiresSuccessfulHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"data":[]}`, http.StatusBadGateway)
	}))
	defer server.Close()
	original := viper.GetString("update_price_service")
	viper.Set("update_price_service", server.URL)
	t.Cleanup(func() { viper.Set("update_price_service", original) })

	if _, err := GetPriceByPriceService(); err == nil {
		t.Fatal("non-success price service response was accepted")
	}
}

func TestRemotePriceCatalogHasBoundedResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", int(MaxRemotePriceCatalogBytes)+1)))
	}))
	defer server.Close()
	original := viper.GetString("update_price_service")
	viper.Set("update_price_service", server.URL)
	t.Cleanup(func() { viper.Set("update_price_service", original) })

	if _, err := GetPriceByPriceService(); err == nil {
		t.Fatal("oversized price service response was accepted")
	}
}
