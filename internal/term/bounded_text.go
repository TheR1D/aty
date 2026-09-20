//go:build darwin || linux

package term

import "github.com/TheR1D/aty/internal/helpers"

// boundedText is append-only: a fixed prefix plus a rolling suffix of raw bytes.
type boundedText struct {
	head, tail           []byte
	start, count, length int
	truncated            bool
}

type textParts struct {
	head, tail string
	omitted    int
}

func (b *boundedText) reset() {
	b.head, b.tail = b.head[:0], b.tail[:0]
	b.start, b.count, b.length, b.truncated = 0, 0, 0, false
}

func (b *boundedText) append(char byte, limit int) {
	b.length++
	if !b.truncated {
		b.head = append(b.head, char)
		b.bound(limit)
		return
	}
	b.pushTail(char, limit-limit/5)
}

func (b *boundedText) pushTail(char byte, limit int) {
	if limit <= 0 {
		return
	}
	if len(b.tail) != limit {
		tail := make([]byte, limit)
		copy(tail, b.tailText())
		b.tail, b.start = tail, 0
	}
	b.tail[(b.start+b.count)%len(b.tail)] = char
	if b.count == len(b.tail) {
		b.start = (b.start + 1) % len(b.tail)
	} else {
		b.count++
	}
}

func (b *boundedText) appendParts(p textParts, limit int) {
	for i := range len(p.head) {
		b.append(p.head[i], limit)
	}
	if p.omitted > 0 {
		b.truncated = true
		b.bound(limit)
		b.length += p.omitted
		b.count, b.start = 0, 0
	}
	for i := range len(p.tail) {
		b.append(p.tail[i], limit)
	}
}

func (b *boundedText) bound(limit int) {
	limit = max(0, limit)
	if !b.truncated && len(b.head) <= limit {
		return
	}
	head := min(len(b.head), limit/5)
	tail := string(b.head[head:]) + b.tailText()
	room := limit - limit/5
	tail = tail[max(0, len(tail)-room):]
	b.head = b.head[:head]
	b.tail = append(b.tail[:0], tail...)
	b.start, b.count, b.truncated = 0, len(tail), true
}

func (b *boundedText) tailText() string {
	if b.count == 0 {
		return ""
	}
	n := min(b.count, len(b.tail)-b.start)
	return string(b.tail[b.start:b.start+n]) + string(b.tail[:b.count-n])
}

func (b *boundedText) parts() textParts {
	return textParts{string(b.head), b.tailText(), b.length - len(b.head) - b.count}
}

// CRLF is one transcript newline; standalone carriage returns remain output.
func (b *boundedText) trimCR() {
	if b.count > 0 && b.tail[(b.start+b.count-1)%len(b.tail)] == '\r' {
		b.count--
		b.length--
	} else if !b.truncated && len(b.head) > 0 && b.head[len(b.head)-1] == '\r' {
		b.head = b.head[:len(b.head)-1]
		b.length--
	}
}

func (b *boundedText) text(limit int) string {
	p := b.parts()
	if p.omitted == 0 {
		return p.head + p.tail
	}
	return helpers.TruncatedOutput(p.head, p.tail, limit)
}
