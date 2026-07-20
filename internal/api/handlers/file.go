package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/files"
)

const fileMaxBodyBytes = 1 << 20

func NewFileHandler(imageFolders []string) http.HandlerFunc {
	allowed, cfgErr := files.BuildAllowedFolders(imageFolders)
	return func(w http.ResponseWriter, r *http.Request) {
		if cfgErr != nil {
			utils.WriteError(w, http.StatusInternalServerError, "Folder configuration invalid", "FOLDER_CONFIG_INVALID", nil)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, fileMaxBodyBytes)
		defer r.Body.Close()

		var req dto.FileRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}
		if err := ensureEOF(dec); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}

		fullPath, err := files.ResolveFilePath(allowed, req.FolderPath, req.FileName)
		if err != nil {
			if errors.Is(err, files.ErrFolderNotAllowed) || errors.Is(err, files.ErrInvalidPath) {
				utils.WriteError(w, http.StatusBadRequest, "Invalid file path", "INVALID_FILE_PATH", nil)
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, "Failed to resolve file path", "FILE_PATH_ERROR", nil)
			return
		}

		f, err := os.Open(fullPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				utils.WriteError(w, http.StatusNotFound, "File not found", "FILE_NOT_FOUND", nil)
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, "Failed to open file", "FILE_OPEN_ERROR", nil)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			utils.WriteError(w, http.StatusInternalServerError, "Failed to read file info", "FILE_INFO_ERROR", nil)
			return
		}
		if info.IsDir() {
			utils.WriteError(w, http.StatusBadRequest, "Path is a directory", "FILE_NOT_FOUND", nil)
			return
		}

		name := filepath.Base(info.Name())
		http.ServeContent(w, r, name, info.ModTime().Truncate(time.Second), f)
	}
}
