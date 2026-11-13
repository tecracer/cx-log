package main

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/megaproaktiv/agentic-cx-designer-sdk-go/acxd"
)

type printer interface {
	Print(acxd.LogEntry)
}

// textPrinter writes one line per event:
//
//	2026-11-13T14:10:03.123Z  NodeTraversal         key=value key="two words"
type textPrinter struct {
	w       io.Writer
	verbose bool
	color   bool
}

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func (p *textPrinter) Print(e acxd.LogEntry) {
	var b strings.Builder
	b.WriteString(p.paint(ansiDim, e.EventTime.UTC().Format("2006-01-02T15:04:05.000Z")))
	b.WriteString("  ")
	b.WriteString(p.paint(eventColor(e.EventType), padRight(string(e.EventType), 28)))
	writeProps(&b, e.EventProperties)
	if p.verbose {
		writeProps(&b, e.CommonProperties)
	}
	b.WriteString("\n")
	io.WriteString(p.w, strings.TrimRight(b.String(), " \n")+"\n")
}

func (p *textPrinter) paint(code, s string) string {
	if !p.color || code == "" {
		return s
	}
	return code + s + ansiReset
}

func eventColor(t acxd.LogEventType) string {
	s := string(t)
	switch {
	case t == acxd.LogEventTypeError, strings.Contains(s, "Error"), strings.Contains(s, "Fail"),
		strings.Contains(s, "Exception"), t == acxd.LogEventTypeInfiniteLoop:
		return ansiRed
	case strings.Contains(s, "Timeout"), strings.Contains(s, "Hallucination"),
		strings.Contains(s, "Escalation"), strings.Contains(s, "Fallback"), t == acxd.LogEventTypeGuardrailRuleTriggered:
		return ansiYellow
	}
	return ansiCyan
}

func writeProps(b *strings.Builder, props []acxd.LogProperty) {
	for _, p := range props {
		b.WriteString(" ")
		b.WriteString(p.Key)
		b.WriteString("=")
		if p.Value == "" || strings.ContainsAny(p.Value, " \t\n\"=") {
			b.WriteString(strconv.Quote(p.Value))
		} else {
			b.WriteString(p.Value)
		}
	}
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// jsonPrinter writes one JSON object per event (JSON Lines), with
// properties flattened into objects for easy jq use.
type jsonPrinter struct{ w io.Writer }

func (p *jsonPrinter) Print(e acxd.LogEntry) {
	toMap := func(props []acxd.LogProperty) map[string]string {
		if len(props) == 0 {
			return nil
		}
		m := make(map[string]string, len(props))
		for _, pr := range props {
			m[pr.Key] = pr.Value
		}
		return m
	}
	_ = json.NewEncoder(p.w).Encode(struct {
		EventTime        string            `json:"eventTime"`
		EventType        acxd.LogEventType `json:"eventType"`
		EventProperties  map[string]string `json:"eventProperties,omitempty"`
		CommonProperties map[string]string `json:"commonProperties,omitempty"`
	}{
		EventTime:        e.EventTime.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		EventType:        e.EventType,
		EventProperties:  toMap(e.EventProperties),
		CommonProperties: toMap(e.CommonProperties),
	})
}
