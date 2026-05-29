// SPDX-License-Identifier: AGPL-3.0-or-later

// Package vision implements the device's camera-callback HTTP endpoint.
//
// Flow:
//
//  1. LLM (mid-turn) calls the self.camera.take_photo tool with a question.
//  2. stackend forwards the MCP tools/call to the device.
//  3. Device captures a JPEG, then POSTs multipart/form-data
//     (question + JPEG) to this endpoint.
//  4. We feed the JPEG + question to a multimodal LLM and stream the
//     reply into a string.
//  5. We return the string as the HTTP body, which the device hands
//     back as the tool result. The original LLM call (still waiting on
//     the tool) then has visual context for the rest of its response.
//
// Two LLM round-trips per take-photo: one to generate the tool_call,
// one to describe the image. With a single multimodal model (gemma4
// with mmproj, qwen3.6-vl etc) that's fine. Future optimization: have
// the device return the JPEG inline and skip the vision endpoint.
//
// Per the firmware (esp32_camera.cc Explain):
//   - Content-Type: multipart/form-data; boundary=----ESP32_CAMERA_BOUNDARY
//   - field "question": text question (UTF-8)
//   - field "file": JPEG image, filename="camera.jpg", Content-Type: image/jpeg
//   - Response: arbitrary text/plain body, used verbatim as the tool result
package vision

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
)

// Config holds the runtime knobs.
type Config struct {
	// LLM is the multimodal chat-completions client. Must point at a
	// model that supports image_url content (gemma4 with mmproj, qwen3.6,
	// etc.). Required.
	LLM *llm.Client

	// MaxImageBytes caps incoming JPEGs. 0 → 8 MiB. The device's max
	// camera resolution rarely exceeds 1 MB but defensive limits keep
	// the server safe from a misbehaving client.
	MaxImageBytes int64

	// SystemPrompt is prepended to the vision call. Empty → use a
	// reasonable default that asks for concise, spoken-friendly output.
	SystemPrompt string

	// Logger receives per-request events. nil → slog.Default().
	Logger *slog.Logger
}

const defaultMaxImage = 8 << 20

const defaultVisionPrompt = `You are describing what's in front of a small desktop robot's camera. Answer the user's question about the image in 1-2 short sentences of natural spoken English. No markdown.`

// Handler returns the HTTP handler. Mount at the URL you advertise
// in MCP capabilities.vision.url.
func Handler(cfg Config) http.HandlerFunc {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxImageBytes == 0 {
		cfg.MaxImageBytes = defaultMaxImage
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = defaultVisionPrompt
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 32MB total request cap including small overhead for the
		// question field and the multipart envelope.
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxImageBytes+(1<<20))

		if err := r.ParseMultipartForm(cfg.MaxImageBytes); err != nil {
			http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
			return
		}

		question := strings.TrimSpace(r.FormValue("question"))
		if question == "" {
			question = "What do you see?"
		}

		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file field: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		jpeg, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "read file: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(jpeg) == 0 {
			http.Error(w, "empty image", http.StatusBadRequest)
			return
		}

		cfg.Logger.Info("vision request",
			"remote", r.RemoteAddr,
			"device_id", r.Header.Get("Device-Id"),
			"question", question,
			"jpeg_bytes", len(jpeg),
		)

		text, err := describe(r.Context(), cfg, question, jpeg)
		if err != nil {
			cfg.Logger.Warn("vision LLM call failed", "err", err)
			http.Error(w, "vision failed", http.StatusBadGateway)
			return
		}
		cfg.Logger.Info("vision response", "text", text)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(text))
	}
}

// describe sends a one-shot multimodal LLM call and concatenates the
// streamed content into a single string for the response body.
func describe(ctx context.Context, cfg Config, question string, jpeg []byte) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: cfg.SystemPrompt},
		llm.UserMultimodal(question, jpeg),
	}

	var sb strings.Builder
	for ev, err := range cfg.LLM.Stream(ctx, msgs, nil) {
		if err != nil {
			return "", err
		}
		if ev.Content != "" {
			sb.WriteString(ev.Content)
		}
		// Tool calls would be unusual here (we passed no tools); ignore.
	}
	return strings.TrimSpace(sb.String()), nil
}
