// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "testing"

func TestIsExitPhrase(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"goodbye", true},
		{"Goodbye!", true},
		{"good night", true},
		{"Goodnight Stackchan", true},
		{"shut down", true},
		{"shutdown", true},
		{"exit", true},
		{"stop listening", true},
		{"that's all for now", true},
		{"see you later", true},
		{"Bye-bye!", false}, // intentional — we don't match hyphenated bye-bye

		{"hello there", false},
		{"what time is it", false},
		{"how do I exit Vim", true}, // false positive we accept — voice users say this rarely
		{"", false},
	}
	for _, c := range cases {
		got := isExitPhrase(c.in)
		if got != c.want {
			t.Errorf("isExitPhrase(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
