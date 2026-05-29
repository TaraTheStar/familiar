// SPDX-License-Identifier: AGPL-3.0-or-later

package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKokoroPCMSendsExpectedRequest(t *testing.T) {
	var (
		gotPath string
		gotCT   string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "audio/pcm")
		// Pretend we returned 480 bytes of audio (= 120 samples = 5ms @24k).
		w.Write(make([]byte, 480))
	}))
	defer srv.Close()

	c := &KokoroClient{BaseURL: srv.URL, Voice: "af_heart", Speed: 0.85, LangCode: "a"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	body, err := c.PCM(ctx, "hello world")
	if err != nil {
		t.Fatalf("PCM: %v", err)
	}
	defer body.Close()

	if gotPath != "/v1/audio/speech" {
		t.Errorf("path=%q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type=%q", gotCT)
	}
	if gotBody["input"] != "hello world" {
		t.Errorf("input=%v", gotBody["input"])
	}
	if gotBody["voice"] != "af_heart" {
		t.Errorf("voice=%v", gotBody["voice"])
	}
	if gotBody["response_format"] != "pcm" {
		t.Errorf("response_format=%v", gotBody["response_format"])
	}
	if gotBody["lang_code"] != "a" {
		t.Errorf("lang_code=%v", gotBody["lang_code"])
	}

	all, _ := io.ReadAll(body)
	if len(all) != 480 {
		t.Errorf("body bytes=%d, want 480", len(all))
	}
}

func TestKokoroPCMPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("model loading"))
	}))
	defer srv.Close()

	c := &KokoroClient{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.PCM(ctx, "test")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "model loading") {
		t.Errorf("error should mention status and body: %v", err)
	}
}

func TestKokoroDefaults(t *testing.T) {
	if defaultStr("", "fallback") != "fallback" {
		t.Error("defaultStr empty")
	}
	if defaultStr("set", "fallback") != "set" {
		t.Error("defaultStr passthrough")
	}
	if defaultF64(0, 1.5) != 1.5 {
		t.Error("defaultF64 zero")
	}
	if defaultF64(0.75, 1.5) != 0.75 {
		t.Error("defaultF64 passthrough")
	}
}
