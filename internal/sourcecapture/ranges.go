package sourcecapture

import (
	"unicode/utf8"

	"github.com/pufferfs/pufferfs/pkg/models"
)

const defaultBoundarySlack = int64(64 << 10)

// MaximumRangeBytes is the largest range the planner can emit for a target.
// The extra byte accounts for a newline immediately after the bounded UTF-8
// boundary search.
func MaximumRangeBytes(targetBytes int64) int64 {
	return targetBytes + defaultBoundarySlack + utf8.UTFMax + 1
}

// RangePlanner derives contiguous, line-aware source ranges while bytes pass
// through another operation such as hashing or uploading. It prefers a newline
// after TargetBytes, but bounds skew by splitting at a UTF-8 boundary.
type RangePlanner struct {
	enabled bool
	target  int64
	slack   int64

	offset     int64
	rangeStart int64
	line       int64
	rangeLine  int64
	completed  []models.SourceRange
}

func NewRangePlanner(enabled bool, targetBytes int64) *RangePlanner {
	if targetBytes < 1 {
		enabled = false
	}
	return &RangePlanner{
		enabled:   enabled,
		target:    targetBytes,
		slack:     defaultBoundarySlack,
		line:      1,
		rangeLine: 1,
	}
}

func (p *RangePlanner) Write(data []byte) (int, error) {
	if !p.enabled {
		p.offset += int64(len(data))
		return len(data), nil
	}
	for i, b := range data {
		absolute := p.offset + int64(i)
		if b == '\n' {
			p.line++
			end := absolute + 1
			if end-p.rangeStart >= p.target {
				p.finishRange(end, p.line)
			}
			continue
		}
		distance := absolute - p.rangeStart
		if distance >= p.target+p.slack && (utf8.RuneStart(b) || distance >= p.target+p.slack+utf8.UTFMax) {
			p.finishRange(absolute, p.line)
		}
	}
	p.offset += int64(len(data))
	return len(data), nil
}

func (p *RangePlanner) finishRange(end, nextLine int64) {
	if end <= p.rangeStart {
		return
	}
	p.completed = append(p.completed, models.SourceRange{
		Offset:    p.rangeStart,
		Length:    end - p.rangeStart,
		LineStart: p.rangeLine,
	})
	p.rangeStart = end
	p.rangeLine = nextLine
}

// Finish returns nil for a source that did not need splitting. Call it only
// after all bytes have been written.
func (p *RangePlanner) Finish() []models.SourceRange {
	if !p.enabled || p.offset == 0 {
		return nil
	}
	p.finishRange(p.offset, p.line)
	if len(p.completed) <= 1 {
		return nil
	}
	ranges := make([]models.SourceRange, len(p.completed))
	copy(ranges, p.completed)
	return ranges
}
