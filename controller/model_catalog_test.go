package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestModelCatalogHandlersPreserveReferenceValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ModelInfo{}, &model.ModelOwnedBy{}); err != nil {
		t.Fatal(err)
	}
	originalDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = originalDB; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	call := func(handler gin.HandlerFunc, body, id string) bool {
		t.Helper()
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if id != "" {
			c.Params = gin.Params{{Key: "id", Value: id}}
		}
		handler(c)
		var response struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.Success && response.Message == "" {
			t.Fatalf("missing failure reason: %s", r.Body.String())
		}
		return response.Success
	}
	if call(CreateModelOwnedBy, `{"id":1000,"name":"reserved"}`, "") {
		t.Fatal("reserved ID accepted")
	}
	if !call(CreateModelOwnedBy, `{"id":1001,"name":"custom"}`, "") {
		t.Fatal("custom ID rejected")
	}
	if call(CreateModelInfo, `{"model":"orphan","owned_by_id":2000}`, "") {
		t.Fatal("unknown owner accepted")
	}
	if !call(CreateModelInfo, `{"model":"visible","owned_by_id":1001}`, "") {
		t.Fatal("valid directory entry rejected")
	}
	if call(CreateModelInfo, `{"model":"visible"}`, "") {
		t.Fatal("duplicate model accepted")
	}
	if call(DeleteModelOwnedBy, "", "1001") {
		t.Fatal("referenced owner deleted")
	}
	info, err := model.GetModelInfoByModel("visible")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(model.ModelInfo{Id: info.Id, Model: info.Model})
	if err != nil {
		t.Fatal(err)
	}
	if !call(UpdateModelInfo, string(payload), "") {
		t.Fatal("clearing attribution rejected")
	}
	if !call(DeleteModelOwnedBy, "", "1001") {
		t.Fatal("unreferenced owner could not be deleted")
	}
	if err := db.Migrator().DropTable(&model.ModelInfo{}); err != nil {
		t.Fatal(err)
	}
	if call(CreateModelInfo, `{"model":"database-failure"}`, "") {
		t.Fatal("database failure reported as success")
	}
}
