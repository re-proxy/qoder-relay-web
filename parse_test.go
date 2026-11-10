package main

import "testing"

func TestParseLegacyLine(t *testing.T) {
	content := `{"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"}`
	evs := parseLegacyLine(content)
	if len(evs) != 1 || evs[0].Type != "content" || evs[0].Text != "hi" {
		t.Fatalf("content parse = %+v", evs)
	}

	reason := `{"body":"{\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}"}`
	evs = parseLegacyLine(reason)
	if len(evs) != 1 || evs[0].Type != "reasoning" || evs[0].Text != "think" {
		t.Fatalf("reasoning parse = %+v", evs)
	}

	tool := `{"body":"{\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{}\"}}]}}]}"}`
	evs = parseLegacyLine(tool)
	if len(evs) != 1 || evs[0].Type != "tool_calls" || len(evs[0].ToolCalls) != 1 || evs[0].ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("tool parse = %+v", evs)
	}

	usage := `{"body":"{\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}"}`
	evs = parseLegacyLine(usage)
	if len(evs) != 1 || evs[0].Type != "usage" || evs[0].Usage == nil || evs[0].Usage.TotalTokens != 15 {
		t.Fatalf("usage parse = %+v", evs)
	}

	gate := `{"statusCode":112,"body":"{\"code\":112}"}`
	evs = parseLegacyLine(gate)
	if len(evs) != 1 || evs[0].Type != "error" || evs[0].Status != 112 {
		t.Fatalf("plan gate parse = %+v", evs)
	}

	if evs := parseLegacyLine("[DONE]"); evs != nil {
		t.Fatalf("done line = %+v", evs)
	}
}
