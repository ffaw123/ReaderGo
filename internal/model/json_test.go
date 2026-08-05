package model

import (
	"encoding/json"
	"testing"
)

func TestJSONDocumentMarshalsAsJSONRatherThanBase64(t *testing.T) {
	document := NewJSONDocument([]map[string]string{{"segment_id": "segment-1"}})
	encoded, err := json.Marshal(struct {
		Citations JSONDocument `json:"citations"`
	}{Citations: document})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"citations":[{"segment_id":"segment-1"}]}` {
		t.Fatalf("encoded JSON = %s", encoded)
	}
}
