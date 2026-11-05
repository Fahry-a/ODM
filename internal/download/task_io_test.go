package download

import "testing"

func TestContentRangeMatches(t *testing.T) {
	tests := []struct {
		name, header      string
		start, end, total int64
		want              bool
	}{
		{"valid", "bytes 0-99/100", 0, 99, 100, true},
		{"wrong start", "bytes 1-99/100", 0, 99, 100, false},
		{"wrong end", "bytes 0-98/100", 0, 99, 100, false},
		{"wrong total", "bytes 0-99/101", 0, 99, 100, false},
		{"wildcard total", "bytes 0-99/*", 0, 99, 100, false},
		{"malformed", "bytes 0-/100", 0, 99, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentRangeMatches(tt.header, tt.start, tt.end, tt.total); got != tt.want {
				t.Fatalf("contentRangeMatches()=%v want=%v", got, tt.want)
			}
		})
	}
}
