package tui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Event is one captured log record.
type Event struct {
	At    time.Time
	Level slog.Level
	Msg   string
	Attrs string
}

// LogSink is an slog.Handler that keeps the last N records in memory.
//
// In TUI mode the logger cannot write to stderr: it would draw over the
// screen. So the records go here instead and the events pane renders them,
// which turns what would have been lost output into the most useful pane on
// the display.
type LogSink struct {
	mu     sync.Mutex
	events []Event
	max    int
	level  slog.Level
	attrs  []slog.Attr
	group  string
	out    io.Writer
}

// NewLogSink returns a sink holding at most max records.
func NewLogSink(max int, level slog.Level) *LogSink {
	return &LogSink{max: max, level: level}
}

// Events returns the last n records, newest last. The UI polls this rather
// than being pushed to, so a burst of log lines cannot block the loop that
// produced them on a renderer that is busy.
func (s *LogSink) Events(n int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.events) {
		n = len(s.events)
	}
	tail := s.events[len(s.events)-n:]
	out := make([]Event, n)
	copy(out, tail)
	return out
}

func (s *LogSink) Enabled(_ context.Context, l slog.Level) bool { return l >= s.level }

func (s *LogSink) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	writeAttr := func(a slog.Attr) {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		key := a.Key
		if s.group != "" {
			key = s.group + "." + key
		}
		fmt.Fprintf(&b, "%s=%v", key, a.Value.Any())
	}
	for _, a := range s.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool { writeAttr(a); return true })

	s.mu.Lock()
	defer s.mu.Unlock()
	e := Event{At: r.Time, Level: r.Level, Msg: r.Message, Attrs: b.String()}
	s.events = append(s.events, e)
	if len(s.events) > s.max {
		s.events = s.events[len(s.events)-s.max:]
	}
	if s.out != nil {
		fmt.Fprintf(s.out, "%s %-5s %s %s\n",
			e.At.Format("15:04:05"), e.Level.String(), e.Msg, e.Attrs)
	}
	return nil
}

// Detach also writes every later record to w.
//
// It exists for the window between the UI handing the terminal back and the
// process exiting. The fan wind-down happens in that window and can take tens
// of seconds against a slow BMC, so an operator watching a blank prompt needs
// to see it happening.
func (s *LogSink) Detach(w io.Writer) {
	s.mu.Lock()
	s.out = w
	s.mu.Unlock()
}

// WithAttrs returns a handler that shares the parent's buffer by pointer, so
// every derived logger lands in the same pane and only the attribute prefix
// differs. Copying the sink itself would copy its mutex with it.
func (s *LogSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &derived{parent: s, attrs: append(append([]slog.Attr{}, s.attrs...), attrs...), group: s.group}
}

func (s *LogSink) WithGroup(name string) slog.Handler {
	return &derived{parent: s, attrs: s.attrs, group: joinGroup(s.group, name)}
}

// derived carries extra attributes but writes into the parent's buffer.
type derived struct {
	parent *LogSink
	attrs  []slog.Attr
	group  string
}

func (d *derived) Enabled(ctx context.Context, l slog.Level) bool { return d.parent.Enabled(ctx, l) }

func (d *derived) Handle(ctx context.Context, r slog.Record) error {
	for i := len(d.attrs) - 1; i >= 0; i-- {
		r.AddAttrs(d.attrs[i])
	}
	return d.parent.Handle(ctx, r)
}

func (d *derived) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &derived{parent: d.parent, attrs: append(append([]slog.Attr{}, d.attrs...), attrs...), group: d.group}
}

func (d *derived) WithGroup(name string) slog.Handler {
	return &derived{parent: d.parent, attrs: d.attrs, group: joinGroup(d.group, name)}
}

func joinGroup(a, b string) string {
	if a == "" {
		return b
	}
	return a + "." + b
}
