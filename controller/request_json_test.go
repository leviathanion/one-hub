package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStrictJSONRequestPreservesNumbersAndRejectsTrailingInput(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{name: "精确大整数与尾部空白", body: `{"value":9007199254740993}` + " \n"},
		{name: "未知字段", body: `{"value":1,"unknown":2}`, wantError: true},
		{name: "第二个对象", body: `{"value":1} {}`, wantError: true},
		{name: "第二个标量", body: `{"value":1} true`, wantError: true},
		{name: "非法尾部", body: `{"value":1} invalid`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			var request struct {
				Value any `json:"value"`
			}
			err := decodeStrictJSONRequest(c, &request, 1024)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected decode result: %v", err)
			}
			if !tc.wantError && request.Value != json.Number("9007199254740993") {
				t.Fatalf("number precision lost: %#v", request.Value)
			}
		})
	}
}

func TestStrictJSONRequestIncludesTrailingWhitespaceInBodyLimit(t *testing.T) {
	const body = `{"value":1}`
	for _, suffix := range []string{"", " "} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body+suffix))
		var request struct {
			Value int `json:"value"`
		}
		err := decodeStrictJSONRequest(c, &request, int64(len(body)))
		if suffix == "" {
			if err != nil {
				t.Fatalf("exact limit rejected: %v", err)
			}
		} else {
			var oversized *http.MaxBytesError
			if !errors.As(err, &oversized) {
				t.Fatalf("oversized trailing input not identified: %v", err)
			}
		}
	}
}
