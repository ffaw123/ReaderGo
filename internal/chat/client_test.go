package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestStreamingTextClientEmitsChatCompletionDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request path = %q", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["stream"] != true {
			t.Fatalf("stream = %#v, want true", request["stream"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, delta := range []string{"你", "好", "！"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", delta)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client, err := newStreamingTextClient("CHAT", server.URL+"/v1", "test-key", "test-model", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	var deltas []string
	content, err := client.complete(context.Background(), "system", "user", false, func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if content != "你好！" {
		t.Fatalf("content = %q", content)
	}
	if !reflect.DeepEqual(deltas, []string{"你", "好", "！"}) {
		t.Fatalf("deltas = %#v", deltas)
	}
}
