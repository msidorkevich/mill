package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeFiles writes files (relative path → content) under dir. Paths must stay inside dir.
func writeFiles(dir string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	if dir == "" {
		return fmt.Errorf("fake runner: step writes files but the request has no Dir")
	}
	for rel, content := range files {
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("fake runner: file path %q escapes the working directory", rel)
		}
		p := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
