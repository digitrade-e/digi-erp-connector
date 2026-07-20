package dto

type FolderFiles struct {
	FolderPath string   `json:"folderPath"`
	Files      []string `json:"files"`
}

type ListFoldersResponse struct {
	Folders []FolderFiles `json:"folders"`
}

type FileRequest struct {
	FolderPath string `json:"folderPath"`
	FileName   string `json:"fileName"`
}
