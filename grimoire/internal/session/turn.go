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
		s.sendError(ctx, "ASR_FAILED", err.Error(), 0)
		return
	}
	s.log.Info("turn: raw transcript", "text", transcript) // pre-filter, for tuning
	transcript = filterASRArtifacts(transcript)
	if transcript == "" {
		s.log.Info("turn: ASR produced no usable text; skipping")
		return
	}
	s.log.Info("turn: transcript", "text", transcript)

	if err := s.out.Transcript(ctx, transcript, true); err != nil {
		s.log.Warn("turn: send transcript failed", "err", err)
	}

	// Drive the avatar to "thinking" while the LLM works.
	_ = s.out.Display(ctx, "thinking", "thinking")

	// Seed dialogue with this user turn.
	s.dialogueMu.Lock()
	s.dialogue = append(s.dialogue, llm.Message{Role: llm.RoleUser, Content: transcript})
	s.dialogueMu.Unlock()

	// Drive the tool loop. It may defer "sleep"-type state changes so the
	// farewell is spoken before the device actually sleeps.
	deferred, err := s.runToolLoop(ctx)
	if err != nil {
		s.log.Warn("turn: loop failed", "err", err)
		// Report LLM/TTS failures to the device (suppressed on a barge-in
		// cancel, where ctx is already done — see sendError).
		s.sendError(ctx, classifyTurnError(err), err.Error(), 0)
	}

	// Barge-in / session teardown: the turn was cancelled. The TTS sink already
	// emitted audio_cancel as it unwound; stop here without the neutral reset,
	// deferred tools, or exit close. Issuing those writes with a cancelled
	// context would fail and make the WS layer tear down the whole connection,
	// and the interrupting turn will set its own display state anyway.
	if ctx.Err() != nil {
		s.log.Info("turn: cancelled (barge-in); skipping wrap-up")
		return
	}

	// Reset emotion when done.
	_ = s.out.Display(ctx, "neutral", "listening")

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
		_ = s.out.Close(ctx, "goodbye")
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

		// Stream one LLM response into a single Speaking session (see
		// streamReply). spoke is true iff any sentence was actually voiced.
		assembled, toolCalls, spoke, err := s.streamReply(ctx, msgs, tools)
		if err != nil {
			return deferred, err
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

// streamReply consumes one LLM stream, voicing assistant text as it arrives
// and collecting any tool calls. The whole text response is one Speaking
// session — a single SpeakBegin … SpeakEnd pair regardless of sentence count —
// so the device never drops to Listening mid-reply and clips later sentences
// (a joke's punchline is the classic casualty; see docs/PROTOCOL_V1.md §5.2).
// SpeakBegin is lazy (first voiced sentence) so a tool-only turn emits no audio
// frames, and SpeakEnd is deferred so the session is always closed — even on a
// stream error — leaving the device out of Speaking.
//
// spoke reports whether any sentence was actually voiced.
func (s *Session) streamReply(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (assembled string, toolCalls []llm.ToolCall, spoke bool, err error) {
	var sb sentenceBuffer

	// begin opens the Speaking session on the first voiced sentence: smile,
	// then SpeakBegin. Idempotent within a reply.
	begin := func() error {
		if spoke {
			return nil
		}
		_ = s.out.Display(ctx, "happy", "speaking")
		if e := s.out.SpeakBegin(ctx); e != nil {
			return e
		}
		spoke = true
		return nil
	}
	speak := func(sentence string) error {
		if e := begin(); e != nil {
			return e
		}
		return s.speakSentence(ctx, sentence)
	}

	// Always end the Speaking session before returning (no-op if never begun).
	// Preserve the first error: a stream/tts failure outranks a close failure.
	defer func() {
		if e := s.out.SpeakEnd(ctx); e != nil && err == nil {
			err = fmt.Errorf("tts close: %w", e)
		}
	}()

	for ev, e := range s.cfg.LLM.Stream(ctx, msgs, tools) {
		if e != nil {
			return assembled, toolCalls, spoke, fmt.Errorf("llm stream: %w", e)
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
			if e := speak(sentence); e != nil {
				return assembled, toolCalls, spoke, fmt.Errorf("tts mid-stream: %w", e)
			}
		}
	}
	// Speak any unterminated trailing fragment.
	if tail := sb.Flush(); tail != "" {
		if e := speak(tail); e != nil {
			return assembled, toolCalls, spoke, fmt.Errorf("tts tail: %w", e)
		}
	}
	return assembled, toolCalls, spoke, nil
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

// dispatchToolCall routes one LLM-emitted tool call: server-side ("local")
// tools are handled in-process, device tools go through the protocol-agnostic
// toolPort (MCP under v1, first-class tool_call under v2). Returns the textual
// result, or an error-message string suitable to feed back to the LLM as the
// tool's "content".
func (s *Session) dispatchToolCall(ctx context.Context, tc llm.ToolCall) (string, error) {
	// Server-side tools (e.g. get_current_time) are handled locally, not
	// forwarded to the device.
	if h, ok := s.localHandlers[tc.Function.Name]; ok {
		s.log.Info("turn: local tool call", "name", tc.Function.Name, "args", tc.Function.Arguments)
		result, err := h(ctx, json.RawMessage(tc.Function.Arguments))
		if err != nil {
			return fmt.Sprintf("Tool failed: %v", err), err
		}
		s.log.Info("turn: local tool result", "name", tc.Function.Name, "result", truncate(result, 120))
		return result, nil
	}

	if s.toolPort == nil {
		return "Tool not available (no device tool registry).", fmt.Errorf("no tool port")
	}
	s.log.Info("turn: tool call", "name", tc.Function.Name, "args", tc.Function.Arguments)

	// LLM gives arguments as a JSON string; the port wants RawMessage.
	args := json.RawMessage(tc.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	text, err := s.toolPort.CallTool(ctx, tc.Function.Name, args)
	if err != nil {
		// Surface error text back to the LLM so it can adapt.
		return fmt.Sprintf("Tool call failed: %v", err), err
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
