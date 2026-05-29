// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "testing"

func TestFilterASRArtifacts(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"*gasp*", ""},
		{"[BLANK_AUDIO]", ""},
		{"(wind blowing)", ""},
		{"♪♪♪", ""},
		{"  *sigh*  ", ""},
		{"", ""},
		{"What time is it?", "What time is it?"},
		{"*gasp* what time is it", "what time is it"},
		{"(laughs) hello there", "hello there"},
		{"turn on the lights [music]", "turn on the lights"},
	}
	for _, c := range cases {
		if got := filterASRArtifacts(c.in); got != c.want {
			t.Errorf("filterASRArtifacts(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
