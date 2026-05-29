// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalToolsAdvertised(t *testing.T) {
	s := &Session{cfg: Config{TimeLocation: time.UTC}}
	tools, handlers := s.buildLocalTools()
	if len(tools) != 1 || tools[0].Function.Name != "get_current_time" {
		t.Fatalf("expected one get_current_time tool, got %+v", tools)
	}
	if _, ok := handlers["get_current_time"]; !ok {
		t.Errorf("get_current_time handler missing")
	}
}

func TestGetCurrentTime(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load tz (tzdata embedded?): %v", err)
	}
	s := &Session{cfg: Config{TimeLocation: loc}}
	out, err := s.getCurrentTime(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Spoken-style timestamp: should carry a clock time (HH:MM) and a comma.
	if !strings.Contains(out, ":") || !strings.Contains(out, ",") {
		t.Errorf("output %q doesn't look like a spoken date/time", out)
	}
}
