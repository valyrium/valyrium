package gateway

import (
	"encoding/json"
	"fmt"
)

// responseFormatInstruction turns a requested response_format into a system
// prompt instruction. The CLI has no native structured-output mode, so
// "support" means prompting for it and validating what comes back.
func responseFormatInstruction(rf *ResponseFormat) string {
	if rf == nil {
		return ""
	}

	switch rf.Type {
	case "json_object":
		return "You must respond with a single valid JSON object and nothing else: no prose, no markdown code fences, no commentary before or after it."
	case "json_schema":
		if rf.JSONSchema == nil || rf.JSONSchema.Schema == nil {
			return "You must respond with a single valid JSON object and nothing else: no prose, no markdown code fences, no commentary before or after it."
		}
		schemaJSON, err := json.Marshal(rf.JSONSchema.Schema)
		if err != nil {
			return "You must respond with a single valid JSON object and nothing else: no prose, no markdown code fences, no commentary before or after it."
		}
		return fmt.Sprintf("You must respond with a single valid JSON value and nothing else (no prose, no markdown code fences, no commentary) that strictly conforms to this JSON schema:\n%s", string(schemaJSON))
	default:
		return ""
	}
}

// requiresJSON reports whether the response_format demands JSON-parseable
// output, and therefore whether the result should be validated/retried.
func requiresJSON(rf *ResponseFormat) bool {
	return rf != nil && (rf.Type == "json_object" || rf.Type == "json_schema")
}

// validateJSONResponse best-effort checks that text satisfies the requested
// response_format: valid JSON in all cases, and a JSON object (not an array
// or scalar) for "json_object" specifically.
func validateJSONResponse(text string, rf *ResponseFormat) error {
	var value interface{}
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return fmt.Errorf("response is not valid JSON: %v", err)
	}

	if rf != nil && rf.Type == "json_object" {
		if _, ok := value.(map[string]interface{}); !ok {
			return fmt.Errorf("response is valid JSON but not a JSON object")
		}
	}

	return nil
}

func jsonRetryPrompt(originalPrompt, priorAttempt string, validationErr error) string {
	return fmt.Sprintf("%s\n\nYour previous reply was:\n%s\n\nThat reply was rejected: %s. Reply again, following the original instructions, with ONLY the corrected JSON and no other text.", originalPrompt, priorAttempt, validationErr.Error())
}
