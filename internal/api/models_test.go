package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type modelListService struct {
	models []aistudio.Model
}

func (s modelListService) Models(context.Context) ([]aistudio.Model, error) { return s.models, nil }

func (modelListService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (modelListService) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event)
	close(events)
	return events, nil
}

func modelTestHandler() http.Handler {
	return NewHandler(modelListService{models: []aistudio.Model{{
		ID:                "gemini-2.5-flash",
		Name:              "Gemini 2.5 Flash",
		Methods:           []string{"generateContent"},
		Capabilities:      map[string]bool{"chat_model": true, "thinking_level": true, "google_search": true},
		CapabilityOptions: map[string][]string{"aliases": {"gemini-flash-latest"}},
	}}}, Config{APIKey: "sk-test"})
}

func getModel(t *testing.T, handler http.Handler, path string, anthropic bool) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer sk-test")
	if anthropic {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s 响应不是 JSON: %q", path, recorder.Body.String())
	}
	return recorder.Code, body
}

// TestRetrieveModel GET /v1/models/{model} 按正式 ID、models/ 前缀、目录别名与后缀别名查找单个模型
func TestRetrieveModel(t *testing.T) {
	handler := modelTestHandler()
	for path, wantID := range map[string]string{
		"/v1/models/gemini-2.5-flash":                   "gemini-2.5-flash",
		"/v1/models/models/gemini-2.5-flash":            "gemini-2.5-flash",
		"/v1/models/gemini-flash-latest":                "gemini-2.5-flash",
		"/v1/models/gemini-2.5-flash-online":            "gemini-2.5-flash-online",
		"/v1/models/gemini-2.5-flash-nothinking-online": "gemini-2.5-flash-nothinking-online",
	} {
		status, body := getModel(t, handler, path, false)
		if status != http.StatusOK || body["id"] != wantID || body["object"] != "model" {
			t.Fatalf("%s: status=%d body=%v", path, status, body)
		}
	}
	status, body := getModel(t, handler, "/v1/models/gemini-2.5-flash", true)
	if status != http.StatusOK || body["type"] != "model" || body["display_name"] != "Gemini 2.5 Flash" {
		t.Fatalf("Anthropic 格式: status=%d body=%v", status, body)
	}
}

// TestRetrieveMissingModel 模型不存在时按协议返回 JSON 404
func TestRetrieveMissingModel(t *testing.T) {
	handler := modelTestHandler()
	status, body := getModel(t, handler, "/v1/models/gpt-4o", false)
	errorBody, _ := body["error"].(map[string]any)
	if status != http.StatusNotFound || errorBody["code"] != "model_not_found" {
		t.Fatalf("OpenAI 404: status=%d body=%v", status, body)
	}
	status, body = getModel(t, handler, "/v1/models/gpt-4o", true)
	errorBody, _ = body["error"].(map[string]any)
	if status != http.StatusNotFound || body["type"] != "error" || errorBody["type"] != "not_found_error" {
		t.Fatalf("Anthropic 404: status=%d body=%v", status, body)
	}
	status, body = getModel(t, handler, "/v1beta/models/gemini-flash-latest", false)
	if status != http.StatusOK || body["name"] != "models/gemini-2.5-flash" {
		t.Fatalf("Gemini 目录别名: status=%d body=%v", status, body)
	}
}

// TestResponseSchemaNullMeansUnset response_format 的 schema 写成 null 时只保留 JSON 模式
func TestResponseSchemaNullMeansUnset(t *testing.T) {
	request := chatRequest{ResponseFormat: json.RawMessage(`{"type":"json_schema","json_schema":{"name":"x","schema":null}}`)}
	config, err := request.generationConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.ResponseMIMEType != "application/json" || config.ResponseSchema != nil {
		t.Fatalf("mime=%q schema=%s", config.ResponseMIMEType, config.ResponseSchema)
	}
}
