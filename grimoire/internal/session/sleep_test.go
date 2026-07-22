// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/TaraTheStar/azoth/llm"
)

func tc(name, args string) llm.ToolCall {
	return llm.ToolCall{Function: llm.FunctionCall{Name: name, Arguments: args}}
}

func TestIsSleepCommand(t *testing.T) {
	cases := []struct {
		tc   llm.ToolCall
		want bool
	}{
		{tc("self.robot.set_state", `{"state":"sleep"}`), true},
		{tc("self.robot.set_state", `{"state":"idle"}`), false},
		{tc("self.robot.set_state", `{"state":"dance"}`), false},
		{tc("self.camera.take_photo", `{"question":"what"}`), false},
		{tc("self.robot.set_state", `not json`), false},
		{tc("self.robot.set_state", ``), false},
	}
	for _, c := range cases {
		if got := isSleepCommand(c.tc); got != c.want {
			t.Errorf("isSleepCommand(%s, %q) = %v, want %v", c.tc.Function.Name, c.tc.Function.Arguments, got, c.want)
		}
	}
}
