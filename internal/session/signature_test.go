package session

import "testing"

func TestNormalizeErrorSignature(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "/Users/me/projectA/main.go:42: undefined: Foo",
			expected: "<file>:<line>: undefined: foo",
		},
		{
			input:    "/home/dev/handler.go:99: undefined: Foo",
			expected: "<file>:<line>: undefined: foo",
		},
		{
			input:    "error at 2026-07-19T14:00:00: connection refused",
			expected: "error at <timestamp>: connection refused",
		},
		{
			input:    "failed with id 123e4567-e89b-12d3-a456-426614174000",
			expected: "failed with id <uuid>",
		},
		{
			input:    "  multiple   spaces   and \t tabs  ",
			expected: "multiple spaces and tabs",
		},
		{
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		actual := NormalizeErrorSignature(tt.input)
		if actual != tt.expected {
			t.Errorf("NormalizeErrorSignature(%q) = %q, expected %q", tt.input, actual, tt.expected)
		}
	}
}
