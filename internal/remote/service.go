package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

type Client interface {
	List(context.Context, weiyun.ListRequest) (weiyun.ListResponse, error)
	Download(context.Context, weiyun.DownloadRequest) (weiyun.DownloadResponse, error)
	Delete(context.Context, weiyun.DeleteRequest) (weiyun.DeleteResponse, error)
	GenShareLink(context.Context, weiyun.GenShareLinkRequest) (weiyun.GenShareLinkResponse, error)
	RenameFile(context.Context, weiyun.RenameFileRequest) (weiyun.RenameFileResponse, error)
	RenameDir(context.Context, weiyun.RenameDirRequest) (weiyun.RenameDirResponse, error)
	CreateDir(context.Context, weiyun.CreateDirRequest) (weiyun.CreateDirResponse, error)
	MoveDir(context.Context, weiyun.MoveDirRequest) (weiyun.MoveDirResponse, error)
	MoveFile(context.Context, weiyun.MoveFileRequest) (weiyun.MoveFileResponse, error)
}

type Uploader interface {
	Upload(context.Context, io.ReaderAt, int64, string, string) (weiyun.UploadResponse, error)
}

type Service struct {
	client     Client
	uploader   Uploader
	downloader *Downloader

	cacheMu     sync.Mutex
	dirTTL      time.Duration
	urlTTL      time.Duration
	dirCache    map[string]cachedDir
	dirInflight map[string]*inflightDir
	urlCache    map[string]cachedURL
	urlInflight map[string]*inflightURL
}

type cachedDir struct {
	listing   Listing
	expiresAt time.Time
}

type inflightDir struct {
	done    chan struct{}
	listing Listing
	err     error
}

type cachedURL struct {
	url       string
	cookie    string
	expiresAt time.Time
}

type inflightURL struct {
	done   chan struct{}
	url    string
	cookie string
	err    error
}

func New(client Client, uploader Uploader, downloader *Downloader) *Service {
	if downloader == nil {
		downloader = &Downloader{}
	}
	return &Service{client: client, uploader: uploader, downloader: downloader}
}

func (s *Service) SetCacheTTL(dirTTL, urlTTL time.Duration) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.dirTTL = dirTTL
	s.urlTTL = urlTTL
	if dirTTL > 0 && s.dirCache == nil {
		s.dirCache = make(map[string]cachedDir)
		s.dirInflight = make(map[string]*inflightDir)
	}
	if urlTTL > 0 && s.urlCache == nil {
		s.urlCache = make(map[string]cachedURL)
		s.urlInflight = make(map[string]*inflightURL)
	}
}

func parentDirPath(normalized string) string {
	parent, _ := path.Split(normalized)
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		return "/"
	}
	return parent
}

func (s *Service) invalidateDir(dirPath string) {
	s.cacheMu.Lock()
	if s.dirCache != nil {
		delete(s.dirCache, dirPath)
	}
	s.cacheMu.Unlock()
}

func (s *Service) invalidateSubtree(normalized string) {
	parent := parentDirPath(normalized)
	prefix := strings.TrimSuffix(normalized, "/") + "/"
	s.cacheMu.Lock()
	if s.dirCache != nil {
		delete(s.dirCache, parent)
		delete(s.dirCache, normalized)
		for k := range s.dirCache {
			if strings.HasPrefix(k, prefix) {
				delete(s.dirCache, k)
			}
		}
	}
	s.cacheMu.Unlock()
}

func (s *Service) Mkdir(ctx context.Context, rawPath string) (string, error) {
	normalized, err := Normalize(rawPath)
	if err != nil {
		return "", err
	}
	if normalized == "/" {
		return "", ErrRootOperation
	}
	parent, name, listing, err := s.parentListing(ctx, normalized)
	if err != nil {
		return "", err
	}
	if _, err := uniqueEntry(normalized, name, listing); !errors.Is(err, ErrNotFound) {
		if err == nil {
			return "", ErrConflict
		}
		return "", err
	}
	response, err := s.client.CreateDir(ctx, weiyun.CreateDirRequest{PDirKey: listing.ParentKey, DirName: name})
	if err != nil {
		return "", err
	}
	if response.DirName != name {
		return "", fmt.Errorf("%w: upstream created %q instead of %q", ErrConflict, response.DirName, name)
	}
	s.invalidateDir(parent.path)
	return path.Join(parent.path, response.DirName), nil
}

func (s *Service) Upload(ctx context.Context, localPath, remotePath string) error {
	if s.uploader == nil {
		return errors.New("upload is unavailable")
	}
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("local source is not a regular file")
	}

	normalized, err := Normalize(remotePath)
	if err != nil {
		return err
	}
	var parentKey, filename, targetDir string
	if normalized == "/" {
		listing, err := s.List(ctx, remotePath)
		if err != nil {
			return err
		}
		parentKey = listing.ParentKey
		targetDir = "/"
		filename = filepath.Base(localPath)
		if _, err := uniqueEntry(path.Join(normalized, filename), filename, listing); !errors.Is(err, ErrNotFound) {
			if err == nil {
				return ErrConflict
			}
			return err
		}
	} else {
		entry, resolveErr := s.Resolve(ctx, remotePath)
		switch {
		case resolveErr == nil && entry.Dir != nil:
			listing, err := s.List(ctx, remotePath)
			if err != nil {
				return err
			}
			parentKey = listing.ParentKey
			targetDir = normalized
			filename = filepath.Base(localPath)
			if _, err := uniqueEntry(path.Join(normalized, filename), filename, listing); !errors.Is(err, ErrNotFound) {
				if err == nil {
					return ErrConflict
				}
				return err
			}
		case resolveErr == nil:
			return ErrConflict
		case !errors.Is(resolveErr, ErrNotFound):
			return resolveErr
		default:
			parent, name, listing, err := s.parentListing(ctx, normalized)
			if err != nil {
				return err
			}
			parentKey = listing.ParentKey
			targetDir = parent.path
			filename = name
		}
	}
	response, err := s.uploader.Upload(ctx, file, info.Size(), filename, parentKey)
	if err != nil {
		return err
	}
	s.invalidateDir(targetDir)
	if response.Filename != "" && response.Filename != filename {
		return fmt.Errorf("%w: upstream stored %q instead of %q", ErrConflict, response.Filename, filename)
	}
	return nil
}

func (s *Service) Download(ctx context.Context, remotePath, localPath string) error {
	entry, err := s.Resolve(ctx, remotePath)
	if err != nil {
		return err
	}
	if entry.File == nil {
		return errors.New("remote source is not a file")
	}
	if info, err := os.Stat(localPath); err == nil {
		if info.IsDir() {
			localPath = filepath.Join(localPath, entry.Name)
		} else {
			return ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(localPath); err == nil {
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	response, err := s.client.Download(ctx, weiyun.DownloadRequest{Items: []weiyun.DownloadFileItem{{FileID: entry.File.FileID, PDirKey: entry.ParentKey}}})
	if err != nil {
		return err
	}
	if len(response.Items) != 1 || response.Items[0].FileID != entry.File.FileID {
		return errors.New("invalid download response")
	}
	item := response.Items[0]
	return s.downloader.Download(ctx, item.HTTPSDownloadURL, item.Cookie, item.FileSize, localPath)
}

func (s *Service) Move(ctx context.Context, sourcePath, destinationPath string) error {
	sourceNormalized, err := Normalize(sourcePath)
	if err != nil {
		return err
	}
	destinationNormalized, err := Normalize(destinationPath)
	if err != nil {
		return err
	}
	if sourceNormalized == "/" || destinationNormalized == "/" {
		return ErrRootOperation
	}
	source, err := s.Resolve(ctx, sourcePath)
	if err != nil {
		return err
	}
	if sourceNormalized == destinationNormalized {
		return nil
	}
	destinationParentPath, destinationName := path.Split(destinationNormalized)
	destinationParentPath = strings.TrimSuffix(destinationParentPath, "/")
	if destinationParentPath == "" {
		destinationParentPath = "/"
	}
	if source.Dir != nil && (destinationParentPath == sourceNormalized || strings.HasPrefix(destinationParentPath, sourceNormalized+"/")) {
		return errors.New("cannot move a directory into itself")
	}
	destinationParent, err := s.resolveDir(ctx, destinationParentPath)
	if err != nil {
		return err
	}
	destinationListing, err := s.listAll(ctx, destinationParent)
	if err != nil {
		return err
	}
	if _, err := uniqueEntry(destinationNormalized, destinationName, destinationListing); !errors.Is(err, ErrNotFound) {
		if err == nil {
			return ErrConflict
		}
		return err
	}
	sourceParentPath, _ := path.Split(sourceNormalized)
	sourceParentPath = strings.TrimSuffix(sourceParentPath, "/")
	if sourceParentPath == "" {
		sourceParentPath = "/"
	}
	if sourceParentPath == destinationParentPath {
		if source.File != nil {
			_, err = s.client.RenameFile(ctx, weiyun.RenameFileRequest{FileID: source.File.FileID, PDirKey: source.ParentKey, NewFilename: destinationName})
		} else {
			_, err = s.client.RenameDir(ctx, weiyun.RenameDirRequest{DirKey: source.Dir.DirKey, PDirKey: source.ParentKey, NewDirName: destinationName, SrcDirName: source.Name})
		}
		if err == nil {
			s.invalidateSubtree(sourceNormalized)
		}
		return err
	}
	if destinationName != source.Name {
		return ErrCrossDirectoryRename
	}
	if source.File != nil {
		_, err = s.client.MoveFile(ctx, weiyun.MoveFileRequest{FileID: source.File.FileID, SrcPDirKey: source.ParentKey, DstPDirKey: destinationListing.ParentKey, Filename: source.Name})
	} else {
		_, err = s.client.MoveDir(ctx, weiyun.MoveDirRequest{DirKey: source.Dir.DirKey, SrcPDirKey: source.ParentKey, DstPDirKey: destinationListing.ParentKey, DirName: source.Name})
	}
	if err == nil {
		s.invalidateSubtree(sourceNormalized)
		s.invalidateDir(destinationParentPath)
	}
	return err
}

func (s *Service) Share(ctx context.Context, rawPath string) (string, error) {
	entry, err := s.Resolve(ctx, rawPath)
	if err != nil {
		return "", err
	}
	if entry.Path == "/" {
		return "", ErrRootOperation
	}
	request := weiyun.GenShareLinkRequest{ShareName: entry.Name}
	if entry.File != nil {
		request.FileList = []weiyun.ShareFileItem{{FileID: entry.File.FileID, PDirKey: entry.ParentKey}}
	} else {
		request.DirList = []weiyun.ShareDirItem{{DirKey: entry.Dir.DirKey, PDirKey: entry.ParentKey}}
	}
	response, err := s.client.GenShareLink(ctx, request)
	if err != nil {
		return "", err
	}
	if response.ShortURL == "" {
		return "", errors.New("empty share link")
	}
	return response.ShortURL, nil
}

func (s *Service) Delete(ctx context.Context, rawPath string, permanently bool) error {
	normalized, err := Normalize(rawPath)
	if err != nil {
		return err
	}
	if normalized == "/" {
		return ErrRootOperation
	}
	entry, err := s.Resolve(ctx, rawPath)
	if err != nil {
		return err
	}
	request := weiyun.DeleteRequest{DeleteCompletely: permanently}
	if entry.File != nil {
		request.FileList = []weiyun.DeleteFileItem{{FileID: entry.File.FileID, PDirKey: entry.ParentKey}}
	} else {
		request.DirList = []weiyun.DeleteDirItem{{DirKey: entry.Dir.DirKey, PDirKey: entry.ParentKey}}
	}
	_, err = s.client.Delete(ctx, request)
	if err == nil {
		s.invalidateSubtree(normalized)
	}
	return err
}

func (s *Service) parentListing(ctx context.Context, normalized string) (dirRef, string, Listing, error) {
	parentPath, name := path.Split(normalized)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}
	parent, err := s.resolveDir(ctx, parentPath)
	if err != nil {
		return dirRef{}, "", Listing{}, err
	}
	listing, err := s.listAll(ctx, parent)
	return parent, name, listing, err
}
