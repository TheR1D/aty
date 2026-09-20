package helpers

// Value returns what p points to, or T's zero value when p is nil.
func Value[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

const OutputTruncationMarker = "\n\n# --- truncated long output ---\n\n"

// TruncatedOutput joins retained text with a 20/80 byte split. The marker
// counts toward the budget. Callers use this only when output was omitted.
func TruncatedOutput(head, tail string, limit int) string {
	if limit < len(OutputTruncationMarker) {
		return ""
	}
	room := limit - len(OutputTruncationMarker)
	return head[:min(len(head), room/5)] + OutputTruncationMarker + tail[max(0, len(tail)-(room-room/5)):]
}
