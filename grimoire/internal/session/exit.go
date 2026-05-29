// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"regexp"
	"strings"
)

// exitPhrasePattern matches user transcripts that should end the session.
// Anchored to whole-word boundaries to avoid false positives like
// "bye bye" inside a normal sentence — though for a voice loop that's
// fine because the firmware idles itself on WS close.
//
// Add phrases here, not in 50 places.
var exitPhrasePattern = regexp.MustCompile(
	`(?i)\b(goodbye|good ?night|see you later|good ?bye|bye ?bye|shut ?down|exit|stop listening|that's all|that's it)\b`,
)

// isExitPhrase returns true if the user's transcript indicates they
// want to end the session.
func isExitPhrase(transcript string) bool {
	t := strings.TrimSpace(transcript)
	if t == "" {
		return false
	}
	return exitPhrasePattern.MatchString(t)
}
