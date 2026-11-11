package weiyun

type ReqHeader struct {
	QUA     string `json:"qua"`
	Version string `json:"version"`
}

type ListRequest struct {
	GetType uint32 `json:"get_type,omitempty"`
	Offset  uint32 `json:"offset,omitempty"`
	Limit   uint32 `json:"limit"`
	OrderBy uint32 `json:"order_by,omitempty"`
	Asc     bool   `json:"asc,omitempty"`
	DirKey  string `json:"dir_key,omitempty"`
	PDirKey string `json:"pdir_key,omitempty"`
}

type ListResponse struct {
	PDirKey        string     `json:"pdir_key"`
	TotalDirCount  uint32     `json:"total_dir_count"`
	TotalFileCount uint32     `json:"total_file_count"`
	DirList        []DirItem  `json:"dir_list"`
	FileList       []FileItem `json:"file_list"`
	FinishFlag     bool       `json:"finish_flag"`
	Error          string     `json:"error"`
}

type DirItem struct {
	DirKey   string `json:"dir_key"`
	DirName  string `json:"dir_name"`
	DirCTime int64  `json:"dir_ctime"`
	DirMTime int64  `json:"dir_mtime"`
}

type FileItem struct {
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	FileSize  int64  `json:"file_size"`
	FileCTime int64  `json:"file_ctime"`
	FileMTime int64  `json:"file_mtime"`
	PDirKey   string `json:"pdir_key"`
}

type ListByCategoryRequest struct {
	CategoryID   uint64   `json:"category_id,omitempty"`
	LibID        int32    `json:"lib_id,omitempty"`
	LocalVersion string   `json:"local_version,omitempty"`
	GroupID      int32    `json:"group_id,omitempty"`
	SuffixList   []string `json:"suffix_list,omitempty"`
	Count        int32    `json:"count"`
	SortType     int32    `json:"sort_type,omitempty"`
	IsDescOrder  bool     `json:"is_desc_order,omitempty"`
}

type ListByCategoryResponse struct {
	ServerVersion string     `json:"server_version"`
	FinishFlag    bool       `json:"finish_flag"`
	FileList      []FileItem `json:"file_list"`
	Error         string     `json:"error"`
}

type DownloadRequest struct {
	Items []DownloadFileItem `json:"items"`
}

type DownloadFileItem struct {
	FileID  string `json:"file_id"`
	PDirKey string `json:"pdir_key"`
}

type DownloadResponse struct {
	Items []DownloadResultItem `json:"items"`
	Error string               `json:"error"`
}

type DownloadResultItem struct {
	FileID           string `json:"file_id"`
	HTTPSDownloadURL string `json:"https_download_url"`
	FileSize         int64  `json:"file_size"`
	Cookie           string `json:"cookie"`
}

type DeleteRequest struct {
	FileList         []DeleteFileItem `json:"file_list,omitempty"`
	DirList          []DeleteDirItem  `json:"dir_list,omitempty"`
	DeleteCompletely bool             `json:"delete_completely,omitempty"`
}

type DeleteFileItem struct {
	FileID  string `json:"file_id"`
	PDirKey string `json:"pdir_key"`
}

type DeleteDirItem struct {
	DirKey  string `json:"dir_key"`
	PDirKey string `json:"pdir_key"`
}

type DeleteResponse struct {
	FreedSpace    int64  `json:"freed_space"`
	FreedIndexCnt uint32 `json:"freed_index_cnt"`
	Error         string `json:"error"`
}

type UploadRequest struct {
	Filename     string        `json:"filename"`
	FileSize     uint64        `json:"file_size,omitempty"`
	FileSHA      string        `json:"file_sha,omitempty"`
	FileMD5      string        `json:"file_md5,omitempty"`
	BlockSHAList []string      `json:"block_sha_list"`
	CheckSHA     string        `json:"check_sha,omitempty"`
	CheckData    string        `json:"check_data,omitempty"`
	PDirKey      string        `json:"pdir_key,omitempty"`
	UploadKey    string        `json:"upload_key,omitempty"`
	ChannelList  []ChannelInfo `json:"channel_list,omitempty"`
	ChannelID    uint32        `json:"channel_id,omitempty"`
	Ex           string        `json:"ex,omitempty"`
	FileData     string        `json:"file_data,omitempty"`
}

type ChannelInfo struct {
	ID     uint32 `json:"id"`
	Offset uint64 `json:"offset"`
	Len    uint32 `json:"len"`
}

type UploadResponse struct {
	FileID      string        `json:"file_id"`
	Filename    string        `json:"filename"`
	FileExist   bool          `json:"file_exist"`
	UploadState int32         `json:"upload_state"`
	UploadKey   string        `json:"upload_key"`
	ChannelList []ChannelInfo `json:"channel_list"`
	Ex          string        `json:"ex"`
	Error       string        `json:"error"`
}

type GenShareLinkRequest struct {
	FileList  []ShareFileItem `json:"file_list,omitempty"`
	DirList   []ShareDirItem  `json:"dir_list,omitempty"`
	ShareName string          `json:"share_name,omitempty"`
	Passwd    string          `json:"passwd,omitempty"`
}

type ShareFileItem struct {
	FileID  string `json:"file_id"`
	PDirKey string `json:"pdir_key"`
}

type ShareDirItem struct {
	DirKey  string `json:"dir_key"`
	PDirKey string `json:"pdir_key"`
}

type GenShareLinkResponse struct {
	ShortURL  string `json:"short_url"`
	ShareName string `json:"share_name"`
	Error     string `json:"error"`
}

type RenameFileRequest struct {
	FileID      string `json:"file_id"`
	PDirKey     string `json:"pdir_key"`
	NewFilename string `json:"new_filename"`
}

type RenameFileResponse struct {
	Error string `json:"error"`
}

type RenameDirRequest struct {
	DirKey     string `json:"dir_key"`
	PDirKey    string `json:"pdir_key"`
	NewDirName string `json:"new_dir_name"`
	SrcDirName string `json:"src_dir_name"`
}

type RenameDirResponse struct {
	Error string `json:"error"`
}

type CreateDirRequest struct {
	PDirKey string `json:"pdir_key,omitempty"`
	DirName string `json:"dir_name"`
}

type CreateDirResponse struct {
	DirKey  string `json:"dir_key"`
	DirName string `json:"dir_name"`
	Error   string `json:"error"`
}

type MoveDirRequest struct {
	DirKey     string `json:"dir_key"`
	SrcPDirKey string `json:"src_pdir_key"`
	DstPDirKey string `json:"dst_pdir_key"`
	DirName    string `json:"dir_name,omitempty"`
}

type MoveDirResponse struct {
	Error string `json:"error"`
}

type MoveFileRequest struct {
	FileID     string `json:"file_id"`
	SrcPDirKey string `json:"src_pdir_key"`
	DstPDirKey string `json:"dst_pdir_key"`
	Filename   string `json:"filename,omitempty"`
}

type MoveFileResponse struct {
	Error string `json:"error"`
}
