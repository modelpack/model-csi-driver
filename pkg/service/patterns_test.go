package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilePatternMatcher_Match(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{
			name:     "wildcard matches safetensors",
			patterns: []string{"*.safetensors"},
			path:     "model.safetensors",
			want:     true,
		},
		{
			name:     "wildcard does not match json",
			patterns: []string{"*.safetensors"},
			path:     "config.json",
			want:     false,
		},
		{
			name:     "negative pattern overrides positive",
			patterns: []string{"*.safetensors", "!tiktoken.model"},
			path:     "tiktoken.model",
			want:     false,
		},
		{
			name:     "empty patterns matches nothing",
			patterns: []string{},
			path:     "any.file",
			want:     false,
		},
		{
			name:     "directory pattern",
			patterns: []string{".git/*"},
			path:     ".git/config",
			want:     true,
		},
		{
			name:     "nested directory pattern",
			patterns: []string{"**/*.bin"},
			path:     "layers/model-00001.bin",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher, err := NewFilePatternMatcher(tt.patterns)
			if err != nil {
				t.Fatalf("NewFilePatternMatcher() error = %v", err)
			}
			if got := matcher.Match(tt.path); got != tt.want {
				t.Errorf("FilePatternMatcher.Match() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilePatternMatcher_Excludes(t *testing.T) {
	t.Run("empty patterns returns false", func(t *testing.T) {
		matcher, err := NewFilePatternMatcher([]string{})
		if err != nil {
			t.Fatalf("NewFilePatternMatcher() error = %v", err)
		}
		if matcher.Excludes() {
			t.Error("Excludes() should return false for empty patterns")
		}
	})

	t.Run("non-empty patterns returns true", func(t *testing.T) {
		matcher, err := NewFilePatternMatcher([]string{"*.safetensors"})
		if err != nil {
			t.Fatalf("NewFilePatternMatcher() error = %v", err)
		}
		if !matcher.Excludes() {
			t.Error("Excludes() should return true for non-empty patterns")
		}
	})
}

func TestNewFilePatternMatcher_Validation(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		wantErr  bool
	}{
		{
			name:     "absolute path is rejected",
			patterns: []string{"/absolute/path"},
			wantErr:  true,
		},
		{
			name:     "parent directory reference is rejected",
			patterns: []string{"../escape"},
			wantErr:  true,
		},
		{
			name:     "valid patterns",
			patterns: []string{"*.safetensors", "!tiktoken.model", "config/*"},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFilePatternMatcher(tt.patterns)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewFilePatternMatcher() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFilterFilesByPatterns(t *testing.T) {
	// Create a temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create test files
	testFiles := []string{
		"model.safetensors",
		"model.safetensors.index.json",
		"config.json",
		"tokenizer/tiktoken.model",
		"layers/model-00001.bin",
		".git/config",
	}

	for _, f := range testFiles {
		fullPath := filepath.Join(tmpDir, f)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte("test"), 0644); err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}
	}

	t.Run("excludes safetensors files", func(t *testing.T) {
		matcher, err := NewFilePatternMatcher([]string{"*.safetensors"})
		if err != nil {
			t.Fatalf("NewFilePatternMatcher() error = %v", err)
		}

		excluded, err := filterFilesByPatterns(tmpDir, matcher)
		if err != nil {
			t.Fatalf("filterFilesByPatterns() error = %v", err)
		}

		if len(excluded) != 1 {
			t.Errorf("Expected 1 excluded file, got %d", len(excluded))
		}

		// Verify file was actually deleted
		if _, err := os.Stat(filepath.Join(tmpDir, "model.safetensors")); !os.IsNotExist(err) {
			t.Error("model.safetensors should have been deleted")
		}

		// Verify other files still exist
		if _, err := os.Stat(filepath.Join(tmpDir, "config.json")); err != nil {
			t.Error("config.json should still exist")
		}
	})

	t.Run("negative pattern includes file", func(t *testing.T) {
		// Recreate test files for this subtest
		for _, f := range testFiles {
			fullPath := filepath.Join(tmpDir, f)
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				t.Fatalf("Failed to create directory: %v", err)
			}
			if err := os.WriteFile(fullPath, []byte("test"), 0644); err != nil {
				t.Fatalf("Failed to create file: %v", err)
			}
		}

		matcher, err := NewFilePatternMatcher([]string{"*.bin", "!tokenizer/*"})
		if err != nil {
			t.Fatalf("NewFilePatternMatcher() error = %v", err)
		}

		excluded, err := filterFilesByPatterns(tmpDir, matcher)
		if err != nil {
			t.Fatalf("filterFilesByPatterns() error = %v", err)
		}

		// Only .bin file should be excluded, not tokenizer files
		hasBin := false
		hasTokenizer := false
		for _, f := range excluded {
			if strings.Contains(f, ".bin") {
				hasBin = true
			}
			if strings.Contains(f, "tokenizer") {
				hasTokenizer = true
			}
		}

		if !hasBin {
			t.Error("Expected .bin file to be excluded")
		}
		if hasTokenizer {
			t.Error("Tokenizer file should NOT be excluded due to negative pattern")
		}
	})

	t.Run("removes empty directories", func(t *testing.T) {
		// Recreate test files for this subtest
		for _, f := range testFiles {
			fullPath := filepath.Join(tmpDir, f)
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				t.Fatalf("Failed to create directory: %v", err)
			}
			if err := os.WriteFile(fullPath, []byte("test"), 0644); err != nil {
				t.Fatalf("Failed to create file: %v", err)
			}
		}

		matcher, err := NewFilePatternMatcher([]string{"layers/*"})
		if err != nil {
			t.Fatalf("NewFilePatternMatcher() error = %v", err)
		}

		_, err = filterFilesByPatterns(tmpDir, matcher)
		if err != nil {
			t.Fatalf("filterFilesByPatterns() error = %v", err)
		}

		// Verify empty layers directory was removed
		if _, err := os.Stat(filepath.Join(tmpDir, "layers")); !os.IsNotExist(err) {
			t.Error("Empty layers directory should have been removed")
		}
	})
}
