package queue

import "testing"

func TestRoutingKeyForTask(t *testing.T) {
	tests := map[string]string{
		"prepare": RoutingKeyPrepare, "asr": RoutingKeyASR, "vision": RoutingKeyVision,
		"rag_index": RoutingKeyRAGIndex, "embedding": RoutingKeyEmbedding, "summary": RoutingKeySummary,
	}
	for taskType, want := range tests {
		got, err := RoutingKeyForTask(taskType)
		if err != nil || got != want {
			t.Errorf("RoutingKeyForTask(%q) = %q, %v; want %q", taskType, got, err, want)
		}
	}
	if _, err := RoutingKeyForTask("unknown"); err == nil {
		t.Fatal("RoutingKeyForTask(unknown) should fail")
	}
}

func TestQueueSpecsIncludeEmbeddingStage(t *testing.T) {
	found := false
	for _, spec := range QueueSpecs {
		if spec.TaskType == "embedding" {
			found = spec.RoutingKey == RoutingKeyEmbedding && spec.QueueName == "readergo.video.embedding"
		}
	}
	if !found {
		t.Fatal("embedding queue topology was not registered")
	}
}
