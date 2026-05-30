// SPDX-License-Identifier: AGPL-3.0-or-later

// Command v2client is a reference Protocol v2 device client. It speaks the real
// v2 wire against a running grimoire server, drives one turn from a microphone
// WAV (or silence), and prints what the server sent back — transcript, captions,
// tool calls, audio frame count — optionally dumping the decoded TTS to a WAV.
//
// It is the laptop-side conformance tool for Stage A: prove the v2 server end to
// end (handshake, Opus interop, credit flow, tools, barge-in) before the ESP32
// firmware port, and keep it around as the oracle that port is checked against.
//
// Example:
//
//	v2client -url ws://192.0.2.10:9098/xiaozhi/v1/ -mic question.wav -out reply.wav -v
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/v2client"
)

func main() {
	var (
		url         = flag.String("url", "", "v2 WebSocket endpoint, e.g. ws://192.0.2.10:9098/xiaozhi/v1/")
		micWAV      = flag.String("mic", "", "16kHz mono 16-bit WAV to send as the user's turn (empty = silent turn)")
		outWAV      = flag.String("out", "", "write the decoded server TTS to this WAV")
		toolsFile   = flag.String("tools", "", "JSON file with an array of tool descriptors to answer tool_list with")
		bargeAfter  = flag.Int("barge-after", 0, "send abort (barge-in) after this many received TTS frames (0 = never)")
		creditBatch = flag.Int("credit-batch", 0, "grant audio_credit every N consumed frames (0 = default 8)")
		timeout     = flag.Duration("timeout", 30*time.Second, "overall turn timeout")
		verbose     = flag.Bool("v", false, "log every protocol frame to stderr")
	)
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(2)
	}

	cfg := v2client.Config{
		URL:              *url,
		OutWAV:           *outWAV,
		BargeAfterFrames: *bargeAfter,
		CreditBatch:      *creditBatch,
	}
	if *verbose {
		cfg.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "· "+format+"\n", args...)
		}
	}

	if *micWAV != "" {
		pcm, rate, channels, err := readMic(*micWAV)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		cfg.MicPCM = pcm
		cfg.MicRate = rate
		fmt.Fprintf(os.Stderr, "mic: %s (%dHz, %d ch, %d samples)\n", *micWAV, rate, channels, len(pcm)/2)
	}

	if *toolsFile != "" {
		tools, err := loadTools(*toolsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		cfg.Tools = tools
		fmt.Fprintf(os.Stderr, "tools: %d loaded from %s\n", len(tools), *toolsFile)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	res, err := v2client.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printSummary(res)
}

// readMic reads the mic WAV and enforces the server's mic format (16kHz mono),
// failing with an actionable message rather than sending mispitched audio.
func readMic(path string) (pcm []byte, rate, channels int, err error) {
	pcm, rate, channels, err = v2client.ReadWAV(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if rate != 16000 || channels != 1 {
		return nil, 0, 0, fmt.Errorf("%s is %dHz/%dch; need 16000Hz mono (try: ffmpeg -i in.wav -ar 16000 -ac 1 out.wav)", path, rate, channels)
	}
	return pcm, rate, channels, nil
}

func loadTools(path string) ([]protov2.ToolDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tools []protov2.ToolDescriptor
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return tools, nil
}

func printSummary(res *v2client.TurnResult) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== turn summary ===\n")
	if s := res.ServerHello.Server; s != nil {
		fmt.Fprintf(&b, "server:      %s %s\n", s.Name, s.Version)
	}
	if fc := res.ServerHello.FlowControl; fc != nil {
		fmt.Fprintf(&b, "audio credit: %d frames\n", fc.AudioCreditInitial)
	}
	fmt.Fprintf(&b, "transcript:  %q\n", res.Transcript)
	fmt.Fprintf(&b, "caption:     %q\n", res.FinalCaption)
	fmt.Fprintf(&b, "tts frames:  %d (%d PCM bytes decoded)\n", res.AudioFrames, len(res.AudioPCM))
	if res.ToolListReqs > 0 || len(res.ToolCalls) > 0 {
		fmt.Fprintf(&b, "tools:       %d discovery req, %d call(s)\n", res.ToolListReqs, len(res.ToolCalls))
		for _, tc := range res.ToolCalls {
			fmt.Fprintf(&b, "  called %s(%s)\n", tc.Name, string(tc.Args))
		}
	}
	if res.Cancelled {
		fmt.Fprintf(&b, "barge-in:    audio_cancel received\n")
	}
	if res.Goodbye != nil {
		fmt.Fprintf(&b, "goodbye:     %s\n", res.Goodbye.Reason)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(&b, "ERROR:       %s: %s\n", e.Code, e.Message)
	}
	fmt.Print(b.String())
}
