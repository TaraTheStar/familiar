// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalTextOnlyMessage(t *testing.T) {
	m := Message{Role: RoleUser, Content: "hello"}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != `{"role":"user","content":"hello"}` {
		t.Errorf("got %s", out)
	}
}

func TestMarshalMultimodalUser(t *testing.T) {
	m := UserMultimodal("what is this?", []byte{0xff, 0xd8, 0xff, 0xe0})
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `"content":[{"type":"text","text":"what is this?"}`) {
		t.Errorf("missing text part: %s", out)
	}
	if !strings.Contains(string(out), `"type":"image_url"`) {
		t.Errorf("missing image_url part: %s", out)
	}
	if !strings.Contains(string(out), `"url":"data:image/jpeg;base64,`) {
		t.Errorf("missing data URI: %s", out)
	}
}

func TestMarshalAssistantToolCallOmitsContent(t *testing.T) {
	m := Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: ToolCallFunction{Name: "set_volume", Arguments: `{"volume":60}`},
		}},
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Content field should be absent (or empty) since there's no text.
	if strings.Contains(string(out), `"content":""`) {
		// OK — empty string is acceptable.
	} else if strings.Contains(string(out), `"content"`) {
		t.Errorf("unexpected content field: %s", out)
	}
	if !strings.Contains(string(out), `"tool_calls"`) {
		t.Errorf("missing tool_calls: %s", out)
	}
}

func TestMarshalToolResult(t *testing.T) {
	m := Message{Role: RoleTool, ToolCallID: "call_1", Name: "set_volume", Content: "volume set to 60"}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"role":"tool"`,
		`"tool_call_id":"call_1"`,
		`"name":"set_volume"`,
		`"content":"volume set to 60"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}
