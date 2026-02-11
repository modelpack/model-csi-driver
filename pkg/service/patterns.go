package service

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	gitignore "github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/pkg/errors"
)

// FilePatternMatcher wraps gitignore pattern matching functionality
type FilePatternMatcher struct {
	matcher  gitignore.Matcher
	patterns []string
}

// NewFilePatternMatcher creates a new pattern matcher from a list of gitignore-compatible patterns
func NewFilePatternMatcher(patterns []string) (*FilePatternMatcher, error) {
	// Validate patterns for security
	for _, p := range patterns {
		// Check for absolute paths (starts with / and has more characters)
		if strings.HasPrefix(p, "/") && len(p) > 1 {
			return nil, errors.Errorf("absolute path patterns are not allowed: %s", p)
		}
		if strings.Contains(p, "..") {
			return nil, errors.Errorf("parent directory reference is not allowed: %s", p)
		}
	}

	// Create gitignore matcher from patterns
	// Parse each string pattern into gitignore.Pattern
	var gitPatterns []gitignore.Pattern
	for _, p := range patterns {
		gitPatterns = append(gitPatterns, gitignore.ParsePattern(p, nil))
	}
	matcher := gitignore.NewMatcher(gitPatterns)

	return &FilePatternMatcher{
		matcher:  matcher,
		patterns: patterns,
	}, nil
}

// Match returns true if the given path matches any of the exclusion patterns
func (m *FilePatternMatcher) Match(path string) bool {
	// gitignore matcher expects paths in forward-slash format
	// and uses a slice of strings for path components
	path = filepath.ToSlash(path)
	pathParts := strings.Split(path, "/")
	isDir := strings.HasSuffix(path, "/")
	return m.matcher.Match(pathParts, isDir)
}

// Excludes returns true if any exclusion patterns are defined
func (m *FilePatternMatcher) Excludes() bool {
	return len(m.patterns) > 0
}

// filterFilesByPatterns walks the target directory and removes files matching the exclusion patterns
// Returns a list of excluded file paths (relative to targetDir)
func filterFilesByPatterns(targetDir string, matcher *FilePatternMatcher) ([]string, error) {
	excludedFiles := []string{}

	// First pass: identify and remove matched files
	err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip the target directory itself
		if path == targetDir {
			return nil
		}

		// Get relative path for pattern matching
		relPath, err := filepath.Rel(targetDir, path)
		if err != nil {
			return errors.Wrap(err, "get relative path")
		}

		// Check if file/directory matches exclusion pattern
		if matcher.Match(relPath) {
			if !info.IsDir() {
				logger.Logger().Infof("Excluding file: %s", relPath)
				excludedFiles = append(excludedFiles, relPath)

				// Remove the file
				if err := os.Remove(path); err != nil {
					return errors.Wrapf(err, "remove excluded file: %s", relPath)
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, errors.Wrap(err, "walk directory for pattern matching")
	}

	// Second pass: remove empty directories
	removeEmptyDirectories(targetDir, matcher)

	// Sort excluded files for consistent logging
	sort.Strings(excludedFiles)

	logger.Logger().Infof("Excluded %d file(s) matching patterns", len(excludedFiles))

	return excludedFiles, nil
}

// removeEmptyDirectories removes empty directories that were created after file removal
func removeEmptyDirectories(targetDir string, matcher *FilePatternMatcher) {
	dirsToRemove := []string{}

	// First, find all empty directories
	err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Continue on error
		}

		if info.IsDir() && path != targetDir {
			isEmpty, _ := isDirEmpty(path)
			if isEmpty {
				dirsToRemove = append(dirsToRemove, path)
			}
		}

		return nil
	})

	if err != nil {
		logger.Logger().WithError(err).Warn("Failed to walk directories for cleanup")
		return
	}

	// Remove empty directories in reverse order (deepest first)
	for i := len(dirsToRemove) - 1; i >= 0; i-- {
		dir := dirsToRemove[i]
		if err := os.Remove(dir); err != nil {
			logger.Logger().WithError(err).Warnf("Failed to remove empty directory: %s", dir)
		} else {
			relPath, _ := filepath.Rel(targetDir, dir)
			logger.Logger().Infof("Removed empty directory: %s", relPath)
		}
	}
}

// isDirEmpty checks if a directory is empty
func isDirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer func(f *os.File) {
		err = f.Close()
		if err != nil {
			return
		}
	}(f)

	_, err = f.Readdirnames(1)
	if err == nil {
		return false, nil // Directory is not empty
	}
	if err.Error() == "EOF" {
		return true, nil // Directory is empty
	}
	return false, err // Error reading directory
}
