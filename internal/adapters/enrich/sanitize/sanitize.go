// Package sanitize implements the log processor chain that keeps token spend and
// storage bounded: dedupe, keep signal, compress, cap.
package sanitize

import (
	"regexp"
	"sort"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.LogProcessors.Register("dedupe", func(e *wire.Env) (ports.LogProcessor, error) {
		return Deduper{StripDigits: e.Cfg.Clean.StripDigits}, nil
	})
	wire.LogProcessors.Register("signal", func(e *wire.Env) (ports.LogProcessor, error) {
		return NewSignalFilter(e.Cfg.Clean.MinLevel, e.Cfg.Clean.Keywords), nil
	})
	wire.LogProcessors.Register("compress", func(e *wire.Env) (ports.LogProcessor, error) {
		return Compressor{MaxLineLen: e.Cfg.Clean.MaxLineLen}, nil
	})
	wire.LogProcessors.Register("cap", func(e *wire.Env) (ports.LogProcessor, error) {
		return Cap{Max: e.Cfg.Clean.MaxLines}, nil
	})
}

var (
	reHex        = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	reUUID       = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reNumber     = regexp.MustCompile(`\b\d+\b`)
	reTimestamp  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`)
	reIPv4       = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// Deduper collapses log lines whose normalized form is identical, keeping the
// first occurrence and counting the rest. A thousand copies of the same timeout
// become one line with "(x1000)".
type Deduper struct {
	StripDigits bool
}

// Name implements ports.LogProcessor.
func (Deduper) Name() string { return "dedupe" }

// Process implements ports.LogProcessor.
func (d Deduper) Process(lines []domain.LogLine) []domain.LogLine {
	index := make(map[string]int, len(lines))
	out := make([]domain.LogLine, 0, len(lines))
	for _, l := range lines {
		key := d.normalize(l.Message)
		if pos, ok := index[key]; ok {
			out[pos].Occurrences++
			if l.Timestamp.After(out[pos].Timestamp) {
				out[pos].Timestamp = l.Timestamp
			}
			continue
		}
		if l.Occurrences == 0 {
			l.Occurrences = 1
		}
		index[key] = len(out)
		out = append(out, l)
	}
	return out
}

// normalize erases the parts of a message that make otherwise identical lines
// look unique: timestamps, ids, addresses and (optionally) all numbers.
func (d Deduper) normalize(msg string) string {
	s := strings.ToLower(msg)
	s = reTimestamp.ReplaceAllString(s, "<ts>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reIPv4.ReplaceAllString(s, "<ip>")
	s = reHex.ReplaceAllString(s, "<hex>")
	if d.StripDigits {
		s = reNumber.ReplaceAllString(s, "<n>")
	}
	return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
}

// SignalFilter keeps only lines that carry diagnostic value: at or above the
// minimum level, or matching a signal keyword.
type SignalFilter struct {
	minRank  int
	keywords []string
}

// NewSignalFilter builds the filter from a level name and keyword list.
func NewSignalFilter(minLevel string, keywords []string) SignalFilter {
	lower := make([]string, 0, len(keywords))
	for _, k := range keywords {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			lower = append(lower, k)
		}
	}
	return SignalFilter{minRank: domain.LevelRankOf(minLevel), keywords: lower}
}

// Name implements ports.LogProcessor.
func (SignalFilter) Name() string { return "signal" }

// Process implements ports.LogProcessor.
func (f SignalFilter) Process(lines []domain.LogLine) []domain.LogLine {
	out := make([]domain.LogLine, 0, len(lines))
	for _, l := range lines {
		if f.keep(l) {
			out = append(out, l)
		}
	}
	// If filtering removed everything, keep the original tail rather than send an
	// empty context to the model.
	if len(out) == 0 && len(lines) > 0 {
		start := len(lines) - 10
		if start < 0 {
			start = 0
		}
		return lines[start:]
	}
	return out
}

func (f SignalFilter) keep(l domain.LogLine) bool {
	if rank := l.LevelRank(); rank > 0 && rank >= f.minRank {
		return true
	}
	msg := strings.ToLower(l.Message)
	for _, k := range f.keywords {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// Compressor squeezes each surviving line: collapse whitespace, drop stack frame
// noise, truncate the middle of very long lines.
type Compressor struct {
	MaxLineLen int
}

// Name implements ports.LogProcessor.
func (Compressor) Name() string { return "compress" }

// Process implements ports.LogProcessor.
func (c Compressor) Process(lines []domain.LogLine) []domain.LogLine {
	max := c.MaxLineLen
	if max <= 0 {
		max = 400
	}
	out := make([]domain.LogLine, 0, len(lines))
	for _, l := range lines {
		msg := strings.TrimSpace(reWhitespace.ReplaceAllString(l.Message, " "))
		if len(msg) > max {
			head := max * 2 / 3
			tail := max - head - 5
			msg = msg[:head] + " ... " + msg[len(msg)-tail:]
		}
		l.Message = msg
		if l.Message == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Cap enforces a hard ceiling on how many lines reach storage and the model,
// keeping the most severe first and preserving chronological order within the
// selection.
type Cap struct {
	Max int
}

// Name implements ports.LogProcessor.
func (Cap) Name() string { return "cap" }

// Process implements ports.LogProcessor.
func (c Cap) Process(lines []domain.LogLine) []domain.LogLine {
	max := c.Max
	if max <= 0 {
		max = 60
	}
	if len(lines) <= max {
		return lines
	}
	ranked := append([]domain.LogLine(nil), lines...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].LevelRank() != ranked[j].LevelRank() {
			return ranked[i].LevelRank() > ranked[j].LevelRank()
		}
		return ranked[i].Occurrences > ranked[j].Occurrences
	})
	kept := ranked[:max]
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Timestamp.Before(kept[j].Timestamp) })
	return kept
}
