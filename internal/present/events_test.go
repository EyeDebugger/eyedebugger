// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package present

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func outputEvents(n, textLen int) []api.Event {
	out := make([]api.Event, n)
	for i := range out {
		out[i] = api.Event{Seq: i + 1, Kind: api.EventOutput, Text: strings.Repeat("x", textLen)}
	}

	return out
}

func TestShapeEvents(t *testing.T) {
	t.Parallel()

	one := jsonSize(&outputEvents(1, 100)[0])

	tests := []struct {
		name        string
		events      []api.Event
		budget      int
		newest      bool
		wantFirst   int
		wantLen     int
		wantOmitted int
	}{
		{"everything fits", outputEvents(5, 100), 0, false, 1, 5, 0},
		{"budget keeps the oldest", outputEvents(5, 100), (3*one + one/2) / CharsPerToken, false, 1, 3, 2},
		{"budget keeps the newest", outputEvents(5, 100), (3*one + one/2) / CharsPerToken, true, 3, 3, 2},
		{"at least one", outputEvents(3, 100), 1, false, 1, 1, 2},
		{"none", nil, 10, false, 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kept, omitted := ShapeEvents(tt.events, tt.budget, tt.newest)
			first := 0
			if len(kept) > 0 {
				first = kept[0].Seq
			}

			if len(kept) != tt.wantLen || omitted != tt.wantOmitted || first != tt.wantFirst {
				t.Errorf("kept %d from seq %d, omitted %d; want %d from seq %d, omitted %d",
					len(kept), first, omitted, tt.wantLen, tt.wantFirst, tt.wantOmitted)
			}
		})
	}
}

func TestShapeEventsHardCap(t *testing.T) {
	t.Parallel()

	events := outputEvents(2000, 900)

	kept, omitted := ShapeEvents(events, 0, false)

	total := 0
	for i := range kept {
		total += jsonSize(&kept[i])
	}

	if total > MaxResultBytes || total+jsonSize(&events[len(kept)]) <= MaxResultBytes || len(kept)+omitted != len(events) {
		t.Errorf("kept %d events (%d bytes), omitted %d; want the most that fit %d bytes", len(kept), total, omitted, MaxResultBytes)
	}
}

func TestShapeEventsCutsText(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", EventTextLimit+5)
	events := []api.Event{{Seq: 1, Kind: api.EventOutput, Text: long}, {Seq: 2, Kind: api.EventOutput, Text: "short"}}

	kept, _ := ShapeEvents(events, 0, false)
	if utf8.RuneCountInString(kept[0].Text) != EventTextLimit || !kept[0].Truncated || kept[1].Truncated {
		t.Errorf("text %d runes, truncated %v/%v; want %d, true/false", utf8.RuneCountInString(kept[0].Text), kept[0].Truncated, kept[1].Truncated, EventTextLimit)
	}

	if events[0].Text != long {
		t.Error("ShapeEvents changed its input")
	}
}

func TestCapOutput(t *testing.T) {
	t.Parallel()

	lines := func(n, textLen int) []api.OutputLine {
		out := make([]api.OutputLine, n)
		for i := range out {
			out[i] = api.OutputLine{Seq: i + 1, Category: "stdout", Text: strings.Repeat("y", textLen)}
		}

		return out
	}
	one := jsonSize(&lines(1, 100)[0])

	tests := []struct {
		name        string
		lines       []api.OutputLine
		max         int
		newest      bool
		wantSeqs    []int
		wantOmitted int
	}{
		{"fits", lines(3, 100), 3 * one, false, []int{1, 2, 3}, 0},
		{"oldest", lines(5, 100), 2*one + 1, false, []int{1, 2}, 3},
		{"newest", lines(5, 100), 2*one + 1, true, []int{4, 5}, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kept, omitted := CapOutput(tt.lines, tt.max, tt.newest)

			got := []int{}
			for _, l := range kept {
				got = append(got, l.Seq)
			}

			if len(got) != len(tt.wantSeqs) || omitted != tt.wantOmitted || got[0] != tt.wantSeqs[0] {
				t.Errorf("kept %v omitted %d; want %v, %d", got, omitted, tt.wantSeqs, tt.wantOmitted)
			}
		})
	}

	t.Run("one big chunk is cut", func(t *testing.T) {
		t.Parallel()

		text := "start" + strings.Repeat("é", 1000) + "end"

		kept, omitted := CapOutput([]api.OutputLine{{Seq: 1, Text: text}}, 100, true)
		if omitted != 0 || len(kept) != 1 || len(kept[0].Text) > 100 || !strings.HasSuffix(kept[0].Text, "end") || !utf8.ValidString(kept[0].Text) {
			t.Errorf("newest: kept %q, want the valid last 100 bytes", kept[0].Text)
		}

		kept, _ = CapOutput([]api.OutputLine{{Seq: 1, Text: text}}, 100, false)
		if len(kept[0].Text) > 100 || !strings.HasPrefix(kept[0].Text, "start") || !utf8.ValidString(kept[0].Text) {
			t.Errorf("oldest: kept %q, want the valid first 100 bytes", kept[0].Text)
		}
	})
}
