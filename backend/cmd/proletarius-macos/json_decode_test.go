package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONAcceptsOneBoundedObject(t *testing.T) {
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":7}`))
	recorder := httptest.NewRecorder()
	var body struct {
		Value int `json:"value"`
	}
	if !decodeJSON(recorder, request, &body) || body.Value != 7 {
		t.Fatalf("valid object rejected: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestDecodeJSONRejectsTrailingObject(t *testing.T) {
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":7}{"value":8}`))
	recorder := httptest.NewRecorder()
	var body struct {
		Value int `json:"value"`
	}
	if decodeJSON(recorder, request, &body) {
		t.Fatal("accepted a second JSON object")
	}
	if recorder.Code != 400 {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}
