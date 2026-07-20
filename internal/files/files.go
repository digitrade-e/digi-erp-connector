package files

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var (
	ErrFolderNotAllowed = errors.New("folder not allowed")
	ErrInvalidPath      = errors.New("invalid path")
)

type AllowedFolder struct {
	Original  string
	Canonical string
}

func BuildAllowedFolders(folders []string) ([]AllowedFolder, error) {
	out := make([]AllowedFolder, 0, len(folders))
	for _, folder := range folders {
		trimmed := strings.TrimSpace(folder)
		if trimmed == "" {
			continue
		}
		canonical, err := canonicalizePath(trimmed)
		if err != nil {
			return nil, err
		}
		out = append(out, AllowedFolder{
			Original:  trimmed,
			Canonical: canonical,
		})
	}
	return out, nil
}

func ListFiles(folder string) ([]string, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	return files, nil
}

func ResolveFilePath(allowed []AllowedFolder, folderPath, fileName string) (string, error) {
	trimmedFolder := strings.TrimSpace(folderPath)
	if trimmedFolder == "" {
		return "", ErrInvalidPath
	}
	if strings.TrimSpace(fileName) == "" {
		return "", ErrInvalidPath
	}

	canonicalFolder, err := canonicalizePath(trimmedFolder)
	if err != nil {
		return "", err
	}
	if !isAllowedFolder(allowed, canonicalFolder) {
		return "", ErrFolderNotAllowed
	}

	rel := filepath.Clean(fileName)
	if rel == "." || rel == ".." || filepath.IsAbs(rel) {
		return "", ErrInvalidPath
	}

	fullPath := filepath.Clean(filepath.Join(canonicalFolder, rel))
	if !isWithinBase(canonicalFolder, fullPath) {
		return "", ErrInvalidPath
	}

	if resolved, err := filepath.EvalSymlinks(fullPath); err == nil {
		if !isWithinBase(canonicalFolder, resolved) {
			return "", ErrInvalidPath
		}
		fullPath = resolved
	}

	return fullPath, nil
}

func isAllowedFolder(allowed []AllowedFolder, folder string) bool {
	for _, item := range allowed {
		if pathEqual(item.Canonical, folder) {
			return true
		}
	}
	return false
}

func canonicalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	return abs, nil
}

func pathEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func isWithinBase(base, target string) bool {
	base = normalizeForCompare(base)
	target = normalizeForCompare(target)

	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return false
	}
	return true
}

func normalizeForCompare(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}
