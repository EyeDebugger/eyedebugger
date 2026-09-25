// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// Hit counts and logpoints are emulated for every adapter (netcoredbg has
// neither): the adapter gets a plain breakpoint, and when the program stops
// at one while any breakpoint has a hit condition or a log message, the
// stop filter decides, under execMu, whether the stop is published or the
// program continues (docs/adr/0010).
//
// While the filter runs, the session still looks running to clients: the
// stop is only applied (stops, state, stopped event) when published.
// Execution requests wait for execMu; inspection is refused as for a
// running program. Every adapter error publishes the stop (fail open).

// Logpoint bounds.
const (
	maxLogExprs      = 10
	maxLogValue      = 200
	maxLogMessage    = 1000
	logpointCategory = "logpoint"
)

// reasonBreakpoint is the stop reason of a breakpoint.
const reasonBreakpoint = "breakpoint"

// Descriptions of stops the filter publishes for another reason.
const (
	stepEndedDescription = "the step ended at a breakpoint that doesn't stop here (a logpoint or an unmet hit count)"
	pausedDescription    = "paused at a breakpoint that doesn't stop here (a logpoint or an unmet hit count)"
)

// hitCondition is a parsed --hit: stop at hit n only (==), from hit n on
// (>=), or every nth hit (%).
type hitCondition struct {
	op string // "==", ">=" or "%"
	n  int
}

// parseHit parses N, >=N or %N (N ≥ 1).
func parseHit(s string) (*hitCondition, error) {
	text := strings.TrimSpace(s)
	op := "=="

	for _, prefix := range []string{">=", "%"} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			op, text = prefix, strings.TrimSpace(rest)

			break
		}
	}

	n, err := strconv.Atoi(text)
	if err != nil || n < 1 {
		return nil, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("invalid hit count %q", s),
			"use N (only the Nth hit), >=N (the Nth and later) or %N (every Nth), with N from 1")
	}

	return &hitCondition{op: op, n: n}, nil
}

// holds reports whether the hit condition holds at hit count.
func (h *hitCondition) holds(count int) bool {
	switch h.op {
	case ">=":
		return count >= h.n
	case "%":
		return count%h.n == 0
	default:
		return count == h.n
	}
}

// logPart is literal text or, with expr set, an expression to evaluate.
type logPart struct {
	text string
	expr string
}

// parseLog parses a logpoint message: {expr} parts are evaluated, {{ and }}
// are literal braces.
func parseLog(msg string) ([]logPart, error) {
	if strings.TrimSpace(msg) == "" {
		return nil, logError(msg, "the message is empty")
	}

	var (
		parts []logPart
		text  strings.Builder
		exprs int
	)

	for i := 0; i < len(msg); i++ {
		c := msg[i]

		switch {
		case (c == '{' || c == '}') && i+1 < len(msg) && msg[i+1] == c:
			text.WriteByte(c)
			i++
		case c == '}':
			return nil, logError(msg, "a } has no {")
		case c == '{':
			end := strings.IndexByte(msg[i+1:], '}')
			if end < 0 {
				return nil, logError(msg, "a { is not closed")
			}

			expr := strings.TrimSpace(msg[i+1 : i+1+end])
			if expr == "" {
				return nil, logError(msg, "{} has no expression")
			}

			exprs++
			if exprs > maxLogExprs {
				return nil, logError(msg, fmt.Sprintf("more than %d {expressions}", maxLogExprs))
			}

			if text.Len() > 0 {
				parts = append(parts, logPart{text: text.String()})
				text.Reset()
			}

			parts = append(parts, logPart{expr: expr})
			i += end + 1
		default:
			text.WriteByte(c)
		}
	}

	if text.Len() > 0 {
		parts = append(parts, logPart{text: text.String()})
	}

	return parts, nil
}

func logError(msg, why string) error {
	return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("invalid log message %q: %s", msg, why),
		`write e.g. --log "i={i} total={total}"; {{ and }} print braces`)
}

// emulated reports whether b needs the stop filter.
func (b *breakpoint) emulated() bool { return b.hit != nil || len(b.log) > 0 }

// emulatingLocked reports whether any breakpoint needs the stop filter.
func (s *Session) emulatingLocked() bool {
	for _, list := range s.bps {
		for _, b := range list {
			if b.emulated() {
				return true
			}
		}
	}

	return false
}

// pendingLocked reports whether the stop of generation gen is still the
// program's latest news: no stop or resume since, and not exited.
func (s *Session) pendingLocked(gen int) bool {
	return s.stopGen == gen && s.state != api.StateExited
}

// applyStopLocked makes stop the session's current stop.
func (s *Session) applyStopLocked(stop api.StopInfo) {
	s.stops++
	s.stop = stop
	s.stepping, s.pauseRequested = false, false
	s.setStateLocked(api.StateStopped)
	s.log.append(api.Event{Kind: api.EventStopped, Stop: &stop})
}

// publish applies stop if it is still pending, with desc as its
// description when set.
func (s *Session) publish(gen int, stop api.StopInfo, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.pendingLocked(gen) {
		return
	}

	if desc != "" {
		stop.Description = desc
	}

	s.applyStopLocked(stop)
}

// planRecord is one breakpoint at the stop's location, as the filter sees
// it.
type planRecord struct {
	b *breakpoint
	// condition must be evaluated: the adapter's slot didn't (it differs).
	condition string
	hits      int // before this stop
	hit       *hitCondition
	log       []logPart
}

// decision is what decide makes of a stop.
type decision struct {
	stop bool
	hit  []*planRecord // counted a hit
	logs []*planRecord // print their message
}

// decide is the stop filter's rule. For each record whose condition holds
// (held[i]): a plain breakpoint stops; an emulated one counts a hit and,
// when its hit condition holds at the new count, logs (a logpoint, which
// never stops) or stops.
func decide(records []*planRecord, held []bool) decision {
	var d decision

	for i, r := range records {
		if !held[i] {
			continue
		}

		if r.hit == nil && len(r.log) == 0 {
			d.stop = true

			continue
		}

		d.hit = append(d.hit, r)

		if r.hit != nil && !r.hit.holds(r.hits+1) {
			continue
		}

		if len(r.log) > 0 {
			d.logs = append(d.logs, r)
		} else {
			d.stop = true
		}
	}

	return d
}

// filterStop decides a breakpoint stop of generation gen. It runs on its own
// goroutine (the DAP read goroutine must stay free to deliver the answers)
// under execMu, so no execution request can move the program meanwhile.
func (s *Session) filterStop(gen int, stop api.StopInfo) {
	ctx := s.life

	s.execMu.Lock()
	defer s.execMu.Unlock()

	s.mu.Lock()
	pending := s.pendingLocked(gen)
	s.mu.Unlock()

	if !pending {
		return
	}

	frames, err := s.stack(ctx, stop.ThreadID, 1)
	if err != nil || len(frames) == 0 {
		s.publish(gen, stop, "")

		return
	}

	records := s.planStop(frames[0].Frame)
	if len(records) == 0 {
		s.publish(gen, stop, "")

		return
	}

	held := make([]bool, len(records))
	for i, r := range records {
		held[i] = r.condition == "" || s.conditionHolds(ctx, frames[0].id, r.condition)
	}

	d := decide(records, held)
	messages := make([]string, len(d.logs))

	for i, r := range d.logs {
		messages[i] = s.renderLog(ctx, frames[0].id, r.log)
	}

	s.mu.Lock()
	for _, r := range d.hit {
		r.b.Hits++
	}

	for _, m := range messages {
		s.appendOutputLocked(logpointCategory, m+"\n")
	}

	desc := s.publishReasonLocked(d.stop)
	s.mu.Unlock()

	if d.stop || desc != "" {
		s.publish(gen, stop, desc)
	} else {
		s.autoContinue(ctx, gen, stop)
	}
}

// publishReasonLocked is the description of a stop the filter publishes
// although no breakpoint wants it: a step or a pause ended there. It is ""
// when the breakpoints decide (stop, or continue past it).
func (s *Session) publishReasonLocked(stop bool) string {
	switch {
	case stop:
		return ""
	case s.stepping:
		return stepEndedDescription
	case s.pauseRequested:
		return pausedDescription
	default:
		return ""
	}
}

// autoContinue resumes the program after a stop the filter swallowed; the
// adapter's continued event for it is not logged. A failure publishes the
// stop.
func (s *Session) autoContinue(ctx context.Context, gen int, stop api.StopInfo) {
	s.mu.Lock()
	if !s.pendingLocked(gen) {
		s.mu.Unlock()

		return
	}

	s.execInFlight = true
	s.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	err := s.sendExec(reqCtx, ExecContinue, stop.ThreadID)

	cancel()

	s.mu.Lock()
	s.execInFlight = false
	s.mu.Unlock()

	if err != nil {
		s.publish(gen, stop, "")
	}
}

// planStop returns the breakpoints at frame's location, with what the
// filter needs of each. A breakpoint matches by the source path the adapter
// gave for it, else its file; a file named differently but the same on disk
// (os.SameFile) matches too.
func (s *Session) planStop(fr api.Frame) []*planRecord {
	s.mu.Lock()
	records, keys := s.planLocked(fr, func(b *breakpoint) bool { return samePath(b.source, fr.File) || samePath(b.File, fr.File) })
	s.mu.Unlock()

	if len(records) > 0 || fr.File == "" {
		return records
	}

	target, err := os.Stat(fr.File)
	if err != nil {
		return nil
	}

	var same string

	for _, key := range keys {
		if filepath.Base(key) != filepath.Base(fr.File) {
			continue
		}

		if info, err := os.Stat(key); err == nil && os.SameFile(info, target) {
			same = key

			break
		}
	}

	if same == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	records, _ = s.planLocked(fr, func(b *breakpoint) bool { return b.File == same })

	return records
}

// planLocked returns the line breakpoints at fr.Line that at accepts, and
// every file key.
func (s *Session) planLocked(fr api.Frame, at func(*breakpoint) bool) (records []*planRecord, keys []string) {
	for key, list := range s.bps {
		if key == funcKey {
			continue
		}

		keys = append(keys, key)

		var conds map[slotKey]string

		for _, b := range list {
			if b.Line != fr.Line || !at(b) {
				continue
			}

			if conds == nil {
				conds = map[slotKey]string{}
				for _, sl := range slotsFor(list) {
					conds[sl.slotKey] = sl.condition
				}
			}

			r := &planRecord{b: b, hits: b.Hits, hit: b.hit, log: b.log}
			if b.Condition != conds[b.key()] {
				r.condition = b.Condition
			}

			records = append(records, r)
		}
	}

	return records, keys
}

func samePath(a, b string) bool {
	return a != "" && b != "" && (a == b || filepath.Clean(a) == filepath.Clean(b))
}

// sameFile reports whether a and b name the same file: the same path, or
// the same file on disk. An adapter's frame paths need not be spelled as
// the file keys are: Delve on Windows reports C:/Users/... with forward
// slashes, possibly in another case or without 8.3 short names.
func sameFile(a, b string) bool {
	if samePath(a, b) {
		return true
	}

	if a == "" || b == "" {
		return false
	}

	ia, err := os.Stat(a)
	if err != nil {
		return false
	}

	ib, err := os.Stat(b)

	return err == nil && os.SameFile(ia, ib)
}

// conditionHolds evaluates a breakpoint condition at the stop: it holds if
// it is true, and if it fails to evaluate (as adapters treat a failing
// condition: stop and let the user see).
func (s *Session) conditionHolds(ctx context.Context, fid int, cond string) bool {
	value, err := s.evalRaw(ctx, fid, cond)

	return err != nil || strings.EqualFold(strings.TrimSpace(value), "true")
}

// renderLog renders a logpoint message, evaluating its expressions.
func (s *Session) renderLog(ctx context.Context, fid int, parts []logPart) string {
	var b strings.Builder

	for _, p := range parts {
		if p.expr == "" {
			b.WriteString(p.text)

			continue
		}

		value, err := s.evalRaw(ctx, fid, p.expr)
		if err != nil {
			value = "<error: " + err.Error() + ">"
		}

		b.WriteString(cutValue(value, maxLogValue))
	}

	return cutValue(b.String(), maxLogMessage)
}

// cutValue cuts s to n runes, marking the cut.
func cutValue(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n]) + "…"
}

// evalRaw evaluates expr in frame fid (watch context) and returns the
// adapter's result text.
func (s *Session) evalRaw(ctx context.Context, fid int, expr string) (string, error) {
	body, err := s.evalBody(ctx, fid, expr, contextWatch)

	return body.Result, err
}

// evalBody sends evaluate for expr in frame fid.
func (s *Session) evalBody(ctx context.Context, fid int, expr, evalContext string) (godap.EvaluateResponseBody, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.EvaluateResponse](ctx, s.client, &godap.EvaluateRequest{
		Request:   godap.Request{Command: "evaluate"},
		Arguments: godap.EvaluateArguments{Expression: expr, FrameId: fid, Context: evalContext},
	})
	if err != nil {
		return godap.EvaluateResponseBody{}, adapterErr(err)
	}

	return resp.Body, nil
}
