// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tts contains text-to-speech clients + the pipeline that drives
// the device's playback. Today there's just Kokoro (OpenAI-compatible
// /v1/audio/speech). The pipeline is in pipeline.go.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// KokoroClient talks to a kokoro-fastapi instance. Default endpoint shape
// matches ghcr.io/remsky/kokoro-fastapi-cpu.
type KokoroClient struct {
	// BaseURL is the root of the Kokoro service, e.g. http://kokoro:8880.
	// /v1/audio/speech is appended.
	BaseURL string

	// Voice is the kokoro voice name (e.g. "af_heart", "af_bella").
	Voice string

	// Speed is the playback speed multiplier; 1.0 is natural. 0.75–0.85
	// reads well for the StackChan tinny speaker; >1.0 sounds rushed.
	Speed float64

	// LangCode is the kokoro language hint ("a"=American English,
	// "b"=British, "z"=Chinese, "j"=Japanese). Defaults to "a".
	LangCode string

	// Model name kokoro expects; defaults to "kokoro".
	Model string

	// HTTP is the underlying client; nil → a default with a generous
	// timeout (TTS for a long sentence can take a few seconds).
	HTTP *http.Client
}

// PCM returns raw 16-bit little-endian PCM at Kokoro's native 24kHz mono.
// One blocking call per synthesis; ReadCloser owned by the caller, must
// be closed.
//
// We ask Kokoro for response_format=pcm specifically so we don't have to
// MP3-decode on our side. PCM bytes stream as soon as kokoro starts
// generating, so even though this is a single HTTP request the body
// readability is incremental.
func (c *KokoroClient) PCM(ctx context.Context, text string) (io.ReadCloser, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("tts: KokoroClient.BaseURL is empty")
	}
	body := map[string]any{
		"model":           defaultStr(c.Model, "kokoro"),
		"input":           text,
		"voice":           defaultStr(c.Voice, "af_heart"),
		"response_format": "pcm",
		"speed":           defaultF64(c.Speed, 1.0),
		"lang_code":       defaultStr(c.LangCode, "a"),
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("tts: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/v1/audio/speech", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("tts: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/pcm")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: kokoro request: %w", err)
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("tts: kokoro status %d: %s", resp.StatusCode, errBody)
	}
	return resp.Body, nil
}

// Kokoro's native sample rate. The /v1/audio/speech response_format=pcm
// always returns 24kHz mono regardless of voice.
const KokoroSampleRate = 24000

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func defaultF64(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}
