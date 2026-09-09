package logsafe

import (
	"context"
	"log/slog"
	"strings"
)

const redacted = "[REDACTED]"

// Handler provides a final guard against logging credentials or event bodies.
// Components should still avoid attaching these values in the first place.
type Handler struct {
	next slog.Handler
}

func New(next slog.Handler) *Handler { return &Handler{next: next} }

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(scrub(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for index, attr := range attrs {
		clean[index] = scrub(attr)
	}
	return &Handler{next: h.next.WithAttrs(clean)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name)}
}

func scrub(attr slog.Attr) slog.Attr {
	key := strings.ToLower(attr.Key)
	for _, sensitive := range []string{"payload", "secret", "authorization", "signature", "credential"} {
		if strings.Contains(key, sensitive) {
			return slog.String(attr.Key, redacted)
		}
	}
	if attr.Value.Kind() == slog.KindGroup {
		members := attr.Value.Group()
		for index := range members {
			members[index] = scrub(members[index])
		}
		return slog.Group(attr.Key, attrsToAny(members)...)
	}
	return attr
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, len(attrs))
	for index := range attrs {
		values[index] = attrs[index]
	}
	return values
}
