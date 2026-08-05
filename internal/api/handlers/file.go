package handlers

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/files"
)

func NewFileHandler(imageFolders []string) http.HandlerFunc {
	allowed, cfgErr := files.BuildAllowedFolders(imageFolders)
	return func(w http.ResponseWriter, r *http.Request) {
		if cfgErr != nil {
			respond.Error(w, http.StatusInternalServerError, "Folder configuration invalid", "FOLDER_CONFIG_INVALID", nil)
			return
		}

		var req dto.FileRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			badJSONRequest(w)
			return
		}

		fullPath, err := files.ResolveFilePath(allowed, req.FolderPath, req.FileName)
		if err != nil {
			if errors.Is(err, files.ErrFolderNotAllowed) || errors.Is(err, files.ErrInvalidPath) {
				respond.Error(w, http.StatusBadRequest, "Invalid file path", "INVALID_FILE_PATH", nil)
				return
			}
			respond.Error(w, http.StatusInternalServerError, "Failed to resolve file path", "FILE_PATH_ERROR", nil)
			return
		}

		f, err := os.Open(fullPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				respond.Error(w, http.StatusNotFound, "File not found", "FILE_NOT_FOUND", nil)
				return
			}
			respond.Error(w, http.StatusInternalServerError, "Failed to open file", "FILE_OPEN_ERROR", nil)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			respond.Error(w, http.StatusInternalServerError, "Failed to read file info", "FILE_INFO_ERROR", nil)
			return
		}
		if info.IsDir() {
			respond.Error(w, http.StatusBadRequest, "Path is a directory", "FILE_NOT_FOUND", nil)
			return
		}

		name := filepath.Base(info.Name())
		http.ServeContent(w, r, name, info.ModTime().Truncate(time.Second), f)
	}
}
