// SPDX-License-Identifier: AGPL-3.0-or-later

package vision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TaraTheStar/azoth/llm"
)

// TestVisionEndpointForwardsToLLM: a fake multimodal LLM returns
// "A cat sitting on a windowsill." in response to any multimodal call;
// the vision handler should accept multipart, forward correctly, and
// return that text as the body.
func TestVisionEndpointForwardsToLLM(t *testing.T) {
	var gotBody struct {
		Messages []json.RawMessage `json:"messages"`
	}
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"A cat sitting on a windowsill.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llmServer.Close()

	visionSrv := httptest.NewServer(Handler(Config{
		LLM: &llm.OpenAIClient{Endpoint: llmServer.URL, Model: "multimodal-test"},
	}))
	defer visionSrv.Close()

	// Build a multipart body matching what the firmware sends.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	mw.WriteField("question", "what do you see")
	fw, _ := mw.CreateFormFile("file", "camera.jpg")
	fw.Write([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}) // not a real JPEG, but the handler doesn't validate
	mw.Close()

	req, _ := http.NewRequest("POST", visionSrv.URL, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Device-Id", "aa:bb:cc:dd:ee:ff")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, respBody)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != "A cat sitting on a windowsill." {
		t.Errorf("body=%q, want %q", got, "A cat sitting on a windowsill.")
	}

	if len(gotBody.Messages) != 2 {
		t.Fatalf("LLM saw %d messages, want 2 (system + user-multimodal)", len(gotBody.Messages))
	}
	// Second message should be the multimodal user message with image content.
	userMsg := string(gotBody.Messages[1])
	if !strings.Contains(userMsg, `"role":"user"`) {
		t.Errorf("user msg role wrong: %s", userMsg)
	}
	if !strings.Contains(userMsg, `"type":"image_url"`) {
		t.Errorf("user msg missing image_url: %s", userMsg)
	}
	if !strings.Contains(userMsg, `"text":"what do you see"`) {
		t.Errorf("user msg missing question: %s", userMsg)
	}
}

func TestVisionRejectsNonPost(t *testing.T) {
	srv := httptest.NewServer(Handler(Config{LLM: &llm.OpenAIClient{Endpoint: "http://unused"}}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status=%d, want 405", resp.StatusCode)
	}
}

func TestVisionRejectsEmptyImage(t *testing.T) {
	srv := httptest.NewServer(Handler(Config{LLM: &llm.OpenAIClient{Endpoint: "http://unused"}}))
	defer srv.Close()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	mw.WriteField("question", "what")
	fw, _ := mw.CreateFormFile("file", "empty.jpg")
	_ = fw // no bytes written
	mw.Close()

	req, _ := http.NewRequest("POST", srv.URL, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for empty image", resp.StatusCode)
	}
}
