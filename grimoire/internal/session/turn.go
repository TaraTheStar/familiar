// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

// maxToolIterations caps the LLM→tool→LLM loop. The LLM should reach a
// final response within a handful of tool calls; if it doesn't we bail
// to protect the user from runaway costs / latency.
const maxToolIterations = 6

// handleTurn is the heart of the voice loop. Given the raw PCM bytes
// captured between listen:start and listen:stop, it:
//
//  1. Transcribes via whisper.cpp
//  2. Sends an stt frame to the device (display-only)
//  3. Pushes the user turn into the dialogue history
//  4. Streams the LLM with available tools (the MCP tool list)
//  5. If LLM returns content: chunks into sentences and Speaks
//  6. If LLM returns tool calls: dispatches each via MCP, feeds results
//     back as tool messages, restarts the stream — up to maxToolIterations
//  7. Persists the assembled response so the next turn has context
//
// The whole call is wrapped in a context tied to the session lifetime
// so a WS close cancels in-flight LLM / MCP / TTS work.
func (s *Session) handleTurn(ctx context.Context, micPCM []byte) {
	// When this turn ends, ask the read loop to resume listening. If we sent
	// TTS, the device transitions Speaking→Listening and sends its own fresh
	// listen:start (which clears this flag); if we sent NO reply, the device
	// stays in Listening streaming, and this is what un-sticks the next
	// utterance. Harmless after a sleep (device gated, no frames arrive).
	defer s.rearm.Store(true)

	if s.cfg.ASR == nil || s.cfg.LLM == nil || s.cfg.Kokoro == nil {
		s.log.Warn("voice loop disabled: ASR/LLM/Kokoro missing",
			"asr_configured", s.cfg.ASR != nil,
			"llm_configured", s.cfg.LLM != nil,
			"kokoro_configured", s.cfg.Kokoro != nil)
		return
	}
	if len(micPCM) == 0 {
		s.log.Info("turn: empty mic buffer; skipping")
		return
	}

	// --- ASR ----------------------------------------------------------
	if path, err := s.dumpTurn(micPCM); err != nil {
		s.log.Warn("turn: WAV dump failed", "err", err)
	} else if path != "" {
		s.log.Info("turn: dumped capture", "path", path)
	}

	samples := bytesToInt16(micPCM)
	// Drop the wake-acknowledge beep that bleeds into the mic at the front of
	// the capture (no AEC) — whisper otherwise hears it as "(gasp)" and it can
	// drown a short command.
	if trimmed, droppedMS := stripLeadingBeep(samples, s.cfg.MicAudio.SampleRate, s.cfg.BeepTrim); droppedMS > 0 {
		s.log.Info("turn: trimmed leading beep", "dropped_ms", droppedMS)
		samples = trimmed
	}
	s.log.Info("turn: transcribing", "samples", len(samples),
		"approx_ms", len(samples)*1000/s.cfg.MicAudio.SampleRate)
	transcript, err := s.cfg.ASR.Transcribe(samples)
	if err != nil {
		s.log.Warn("turn: ASR failed", "err", err)
		return
	}
	s.log.Info("turn: raw transcript", "text", transcript) // pre-filter, for tuning
	transcript = filterASRArtifacts(transcript)
	if transcript == "" {
		s.log.Info("turn: ASR produced no usable text; skipping")
		return
	}
	s.log.Info("turn: transcript", "text", transcript)

	if err := writeJSON(ctx, s.conn, protocol.STT{Type: "stt", Text: transcript}); err != nil {
		s.log.Warn("turn: send stt failed", "err", err)
	}

	// Drive the avatar to "thinking" while the LLM works.
	s.setEmotion(ctx, "thinking")

	// Seed dialogue with this user turn.
	s.dialogueMu.Lock()
	s.dialogue = append(s.dialogue, llm.Message{Role: llm.RoleUser, Content: transcript})
	s.dialogueMu.Unlock()

	// Drive the tool loop. It may defer "sleep"-type state changes so the
	// farewell is spoken before the device actually sleeps.
	deferred, err := s.runToolLoop(ctx)
	if err != nil {
		s.log.Warn("turn: loop failed", "err", err)
	}

	// Reset emotion when done.
	s.setEmotion(ctx, "neutral")

	// Now run any deferred state changes (e.g. go-to-sleep). Doing this LAST —
	// after all speech and the neutral reset — means the device talks
	// normally, exits the talking animation, and only then enters sleep, so
	// the sleepy avatar set by the firmware isn't clobbered.
	for _, tc := range deferred {
		s.log.Info("turn: dispatching deferred tool", "name", tc.Function.Name, "args", tc.Function.Arguments)
		if _, derr := s.dispatchToolCall(ctx, tc); derr != nil {
			s.log.Warn("turn: deferred tool failed", "name", tc.Function.Name, "err", derr)
		}
	}

	// Going to sleep ends the conversation. Clear the dialogue history so that
	// when the device is woken (head-pet) and talked to again, the assistant
	// starts fresh — otherwise it reads "I went to sleep" from history and
	// insists it's "already in sleep mode" on the next request.
	if len(deferred) > 0 {
		s.dialogueMu.Lock()
		s.dialogue = nil
		s.dialogueMu.Unlock()
		s.log.Info("turn: cleared dialogue after sleep")
	}

	// Exit handling: if the user said something farewell-ish, close the
	// WS after the response finishes playing so the device returns to
	// idle. Firmware re-opens lazily on the next wake word.
	if isExitPhrase(transcript) {
		s.log.Info("turn: exit phrase detected; closing session", "phrase", transcript)
		_ = s.closeNormal()
	}
}

// setEmotion sends a `llm` frame to drive the avatar. Best-effort — log
// + continue on error since this is cosmetic.
func (s *Session) setEmotion(ctx context.Context, emotion string) {
	if err := writeJSON(ctx, s.conn, protocol.LLM{Type: "llm", Emotion: emotion}); err != nil {
		s.log.Debug("set emotion failed", "emotion", emotion, "err", err)
	}
}

// runToolLoop is the iterative LLM ↔ tools dance. Each iteration sends
// the current dialogue to the LLM with available tools; if the LLM
// returns tool calls, we execute them via MCP, append results, and
// loop. If the LLM returns content, we Speak it and exit.
// runToolLoop returns any tool calls it deliberately deferred (e.g. a
// go-to-sleep state change) for the caller to dispatch after speech finishes.
func (s *Session) runToolLoop(ctx context.Context) ([]llm.ToolCall, error) {
	tools := s.snapshotTools()
	var deferred []llm.ToolCall
	for iter := 0; iter < maxToolIterations; iter++ {
		// Build the message list: system prompt (if any) + dialogue.
		s.dialogueMu.Lock()
		msgs := make([]llm.Message, 0, len(s.dialogue)+1)
		if s.cfg.SystemPrompt != "" {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: s.cfg.SystemPrompt})
		}
		msgs = append(msgs, s.dialogue...)
		s.dialogueMu.Unlock()

		var (
			sb        sentenceBuffer
			assembled string
			toolCalls []llm.ToolCall
			spoke     bool
		)
		// One Speaking session for this whole LLM text response: every
		// sentence streams into a single tts:start … tts:stop so the
		// device never drops to Listening mid-reply and clips later
		// sentences (e.g. a joke's punchline). Closed before we return.
		tts := s.newTTSSession()
		for ev, err := range s.cfg.LLM.Stream(ctx, msgs, tools) {
			if err != nil {
				_ = tts.Close(ctx)
				return deferred, fmt.Errorf("llm stream: %w", err)
			}
			if ev.ToolCall != nil {
				toolCalls = append(toolCalls, *ev.ToolCall)
				continue
			}
			if ev.Content == "" {
				continue
			}
			assembled += ev.Content
			for _, sentence := range sb.Add(ev.Content) {
				if err := tts.Speak(ctx, sentence); err != nil {
					_ = tts.Close(ctx)
					return deferred, fmt.Errorf("tts mid-stream: %w", err)
				}
				spoke = true
			}
		}
		// Speak any unterminated trailing fragment.
		if tail := sb.Flush(); tail != "" {
			if err := tts.Speak(ctx, tail); err != nil {
				_ = tts.Close(ctx)
				return deferred, fmt.Errorf("tts tail: %w", err)
			}
			spoke = true
		}
		// End the Speaking session (drains the lead, sends tts:stop) before
		// dispatching tools or looping back to the LLM.
		if err := tts.Close(ctx); err != nil {
			return deferred, fmt.Errorf("tts close: %w", err)
		}

		// Record what the assistant just said + did.
		s.dialogueMu.Lock()
		s.dialogue = append(s.dialogue, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   assembled,
			ToolCalls: toolCalls,
		})
		s.dialogueMu.Unlock()

		if len(toolCalls) == 0 {
			if !spoke {
				s.log.Warn("turn: LLM returned empty response", "raw", assembled)
			}
			return deferred, nil // happy path: text-only response, done.
		}

		// Dispatch each tool call via MCP.
		for _, tc := range toolCalls {
			// Defer sleep-type state changes until after the turn's speech so
			// the farewell is heard and the talking animation ends before the
			// device sleeps. Feed the LLM a synthetic success so it still
			// generates the farewell this turn.
			if isSleepCommand(tc) {
				s.log.Info("turn: deferring sleep until speech completes", "args", tc.Function.Arguments)
				deferred = append(deferred, tc)
				s.dialogueMu.Lock()
				s.dialogue = append(s.dialogue, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Content:    "ok",
				})
				s.dialogueMu.Unlock()
				continue
			}
			result, callErr := s.dispatchToolCall(ctx, tc)
			s.dialogueMu.Lock()
			s.dialogue = append(s.dialogue, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
			s.dialogueMu.Unlock()
			if callErr != nil {
				s.log.Warn("turn: tool call failed", "tool", tc.Function.Name, "err", callErr)
				// Keep going — the tool message carries the error text
				// so the LLM can react.
			}
		}
		// Loop: send updated dialogue back to LLM.
	}

	s.log.Warn("turn: hit maxToolIterations; giving up", "max", maxToolIterations)
	return deferred, nil
}

// isSleepCommand reports whether a tool call is the device's go-to-sleep
// state change, which we defer until after the turn's speech.
func isSleepCommand(tc llm.ToolCall) bool {
	if tc.Function.Name != "self.robot.set_state" {
		return false
	}
	var args struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return false
	}
	return args.State == "sleep"
}

// dispatchToolCall sends one tool call to the device via MCP and
// returns the textual result (or an error message string suitable for
// feeding back to the LLM as the tool's "content").
func (s *Session) dispatchToolCall(ctx context.Context, tc llm.ToolCall) (string, error) {
	// Server-side tools (e.g. get_current_time) are handled locally, not
	// forwarded over MCP to the device.
	if h, ok := s.localHandlers[tc.Function.Name]; ok {
		s.log.Info("turn: local tool call", "name", tc.Function.Name, "args", tc.Function.Arguments)
		result, err := h(ctx, json.RawMessage(tc.Function.Arguments))
		if err != nil {
			return fmt.Sprintf("Tool failed: %v", err), err
		}
		s.log.Info("turn: local tool result", "name", tc.Function.Name, "result", truncate(result, 120))
		return result, nil
	}

	if s.mcpClient == nil {
		return "Tool not available (MCP not initialized).", fmt.Errorf("mcp not initialized")
	}
	s.log.Info("turn: tool call", "name", tc.Function.Name, "args", tc.Function.Arguments)

	// LLM gives us arguments as a JSON string; MCP wants RawMessage.
	args := json.RawMessage(tc.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	result, err := s.mcpClient.CallTool(ctx, tc.Function.Name, args)
	if err != nil {
		// Surface error text back to the LLM so it can adapt.
		return fmt.Sprintf("Tool call failed: %v", err), err
	}
	text := result.Text()
	if text == "" {
		// Tool ran but returned no text content (e.g. a setter). Tell the
		// LLM it succeeded.
		text = "ok"
	}
	s.log.Info("turn: tool result", "name", tc.Function.Name, "result", truncate(text, 120))
	return text, nil
}

// bytesToInt16 reinterprets little-endian s16 PCM bytes as []int16.
func bytesToInt16(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// asrAnnotationRE matches whisper's non-speech annotations: bracketed
// "[BLANK_AUDIO]", parenthesized "(wind blowing)", asterisked "*gasp*", and
// music notes "♪...♪". Whisper emits these for breaths/silence/noise.
var asrAnnotationRE = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)|\*[^*]*\*|♪[^♪]*♪|♪+`)

// filterASRArtifacts strips whisper's non-speech annotations. If what remains
// has no actual letters/digits it was pure non-speech, so we return "" — which
// makes handleTurn skip the LLM call (and the device stays quiet) rather than
// feeding the model a "*gasp*" it can only answer with silence.
func filterASRArtifacts(s string) string {
	s = strings.TrimSpace(asrAnnotationRE.ReplaceAllString(s, ""))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return s // has real speech content
		}
	}
	return ""
}
