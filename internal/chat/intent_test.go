package chat

import "testing"

func TestNormalizeIntentDecision(t *testing.T) {
	tests := []struct {
		name   string
		input  IntentDecision
		intent string
		reason string
	}{
		{
			name:   "multi intent always clarifies",
			input:  IntentDecision{Intent: IntentVideoDetail, Confidence: .99, IsMultiIntent: true, AmbiguityReason: AmbiguityNone, RewrittenQuery: "query"},
			intent: IntentAmbiguous, reason: AmbiguityMultiIntent,
		},
		{
			name:   "low confidence clarifies",
			input:  IntentDecision{Intent: IntentGeneralChat, Confidence: .6, AmbiguityReason: AmbiguityNone},
			intent: IntentAmbiguous, reason: AmbiguityLowConfidence,
		},
		{
			name:   "detail requires rewritten query",
			input:  IntentDecision{Intent: IntentVideoDetail, Confidence: .9, AmbiguityReason: AmbiguityNone, RequiredCapability: CapabilityRAG},
			intent: IntentAmbiguous, reason: AmbiguityMissingReference,
		},
		{
			name:   "valid overview",
			input:  IntentDecision{Intent: IntentVideoOverview, Confidence: .9, AmbiguityReason: AmbiguityNone},
			intent: IntentVideoOverview, reason: AmbiguityNone,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeIntentDecision(test.input, .75)
			if got.Intent != test.intent || got.AmbiguityReason != test.reason {
				t.Fatalf("normalize = (%s, %s), want (%s, %s)", got.Intent, got.AmbiguityReason, test.intent, test.reason)
			}
		})
	}
}

func TestDecodeIntentDecisionRejectsUnknownFields(t *testing.T) {
	_, err := decodeIntentDecision(`{"intent":"GENERAL_CHAT","confidence":1,"is_multi_intent":false,"ambiguity_reason":"NONE","required_capability":"NONE","resolved_references":{},"rewritten_query":"","unexpected":true}`)
	if err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}
