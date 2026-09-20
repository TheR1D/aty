package term

import "testing"

func TestParseTpgid(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want int
	}{
		{
			name: "plain",
			stat: "412 (bash) S 410 412 412 34816 4711 4194304 1583 0 0 0",
			want: 4711,
		},
		{
			name: "comm with spaces and parens",
			stat: "412 (we (are) legion) S 410 412 412 34816 4711 4194304 1583 0",
			want: 4711,
		},
		{
			name: "no foreground process group",
			stat: "412 (bash) S 410 412 412 0 -1 4194304 1583 0 0 0",
			want: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTpgid([]byte(tt.stat + "\n"))
			if err != nil {
				t.Fatalf("parseTpgid: %v", err)
			}
			if got != tt.want {
				t.Errorf("tpgid = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParsePpid(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want int
	}{
		{
			name: "plain",
			stat: "412 (bash) S 410 412 412 34816 4711 4194304 1583 0 0 0",
			want: 410,
		},
		{
			name: "comm with spaces and parens",
			stat: "412 (we (are) legion) S 410 412 412 34816 4711 4194304 1583 0",
			want: 410,
		},
		{
			name: "init",
			stat: "1 (systemd) S 0 1 1 0 -1 4194304 1583 0 0 0",
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePpid([]byte(tt.stat + "\n"))
			if err != nil {
				t.Fatalf("parsePpid: %v", err)
			}
			if got != tt.want {
				t.Errorf("ppid = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParsePpidRejectsGarbage(t *testing.T) {
	tests := []struct {
		name string
		stat string
	}{
		{name: "empty", stat: ""},
		{name: "no comm", stat: "412 bash S 410 412 412 34816 4711"},
		{name: "truncated", stat: "412 (bash) S"},
		{name: "not a number", stat: "412 (bash) S x 412 412 34816 4711"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parsePpid([]byte(tt.stat)); err == nil {
				t.Errorf("parsePpid(%q) = %d, want error", tt.stat, got)
			}
		})
	}
}

func TestParseTpgidRejectsGarbage(t *testing.T) {
	tests := []struct {
		name string
		stat string
	}{
		{name: "empty", stat: ""},
		{name: "no comm", stat: "412 bash S 410 412 412 34816 4711"},
		{name: "truncated", stat: "412 (bash) S 410 412"},
		{name: "not a number", stat: "412 (bash) S 410 412 412 34816 ? 4194304"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parseTpgid([]byte(tt.stat)); err == nil {
				t.Errorf("parseTpgid(%q) = %d, want error", tt.stat, got)
			}
		})
	}
}
