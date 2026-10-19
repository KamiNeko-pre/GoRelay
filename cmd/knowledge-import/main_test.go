package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadBoundedAcceptsExactLimitAndRejectsOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"below_limit", "abc", false},
		{"exact_limit", "abcd", false},
		{"over_limit", "abcde", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readBounded(path, 4)
			if tc.wantErr {
				if err == nil || got != nil {
					t.Fatalf("oversize input was returned: %q, %v", got, err)
				}
				return
			}
			if err != nil || string(got) != tc.body {
				t.Fatalf("valid file rejected: %q, %v", got, err)
			}
		})
	}
}
