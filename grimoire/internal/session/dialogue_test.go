// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
)

func TestTrimDialogue(t *testing.T) {
	msg := func(role llm.Role) llm.Message { return llm.Message{Role: role, Content: "x"} }

	t.Run("under cap untouched", func(t *testing.T) {
		msgs := []llm.Message{msg(llm.RoleUser), msg(llm.RoleAssistant)}
		got := trimDialogue(msgs, 4)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
	})

	t.Run("cuts at user boundary", func(t *testing.T) {
		// user, assistant(tool_calls), tool, assistant | user, assistant
		msgs := []llm.Message{
			msg(llm.RoleUser),
			msg(llm.RoleAssistant),
			msg(llm.RoleTool),
			msg(llm.RoleAssistant),
			msg(llm.RoleUser),
			msg(llm.RoleAssistant),
		}
		got := trimDialogue(msgs, 3)
		// len-max = 3 → first user at or after index 3 is index 4.
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].Role != llm.RoleUser {
			t.Fatalf("first surviving role = %q, want user", got[0].Role)
		}
	})

	t.Run("no user in tail clears", func(t *testing.T) {
		msgs := []llm.Message{
			msg(llm.RoleUser),
			msg(llm.RoleAssistant),
			msg(llm.RoleTool),
			msg(llm.RoleTool),
			msg(llm.RoleTool),
		}
		got := trimDialogue(msgs, 3)
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
	})
}
