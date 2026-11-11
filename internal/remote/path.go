package remote

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

const pageSize = 50

var (
	ErrInvalidPath          = errors.New("invalid remote path")
	ErrNotFound             = errors.New("remote path not found")
	ErrAmbiguous            = errors.New("remote path is ambiguous")
	ErrNoProgress           = errors.New("remote pagination made no progress")
	ErrConflict             = errors.New("destination already exists")
	ErrRootOperation        = errors.New("operation on authorization root is forbidden")
	ErrCrossDirectoryRename = errors.New("cross-directory move cannot also rename")
)

type Listing struct {
	Path      string
	ParentKey string
	Dirs      []weiyun.DirItem
	Files     []weiyun.FileItem
}

type Entry struct {
	Path      string
	Name      string
	ParentKey string
	Dir       *weiyun.DirItem
	File      *weiyun.FileItem
}

type dirRef struct {
	path      string
	dirKey    string
	parentKey string
}

func Normalize(raw string) (string, error) {
	if !strings.HasPrefix(raw, "wy:/") {
		return "", ErrInvalidPath
	}
	parts := strings.Split(strings.TrimPrefix(raw, "wy:/"), "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(clean) == 0 {
				return "", ErrInvalidPath
			}
			clean = clean[:len(clean)-1]
		default:
			clean = append(clean, part)
		}
	}
	if len(clean) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(clean, "/"), nil
}

func (s *Service) List(ctx context.Context, rawPath string) (Listing, error) {
	normalized, err := Normalize(rawPath)
	if err != nil {
		return Listing{}, err
	}
	dir, err := s.resolveDir(ctx, normalized)
	if err != nil {
		return Listing{}, err
	}
	listing, err := s.listAll(ctx, dir)
	listing.Path = normalized
	return listing, err
}

func (s *Service) Resolve(ctx context.Context, rawPath string) (Entry, error) {
	normalized, err := Normalize(rawPath)
	if err != nil {
		return Entry{}, err
	}
	if normalized == "/" {
		return Entry{Path: "/", Name: "/"}, nil
	}
	parentPath, name := path.Split(normalized)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}
	parent, err := s.resolveDir(ctx, parentPath)
	if err != nil {
		return Entry{}, err
	}
	listing, err := s.listAll(ctx, parent)
	if err != nil {
		return Entry{}, err
	}
	return uniqueEntry(normalized, name, listing)
}

func (s *Service) resolveDir(ctx context.Context, normalized string) (dirRef, error) {
	current := dirRef{path: "/"}
	if normalized == "/" {
		return current, nil
	}
	for _, name := range strings.Split(strings.TrimPrefix(normalized, "/"), "/") {
		listing, err := s.listAll(ctx, current)
		if err != nil {
			return dirRef{}, err
		}
		entry, err := uniqueEntry(path.Join(current.path, name), name, listing)
		if err != nil {
			return dirRef{}, err
		}
		if entry.Dir == nil {
			return dirRef{}, fmt.Errorf("%w: %s is not a directory", ErrInvalidPath, entry.Path)
		}
		current = dirRef{path: entry.Path, dirKey: entry.Dir.DirKey, parentKey: listing.ParentKey}
	}
	return current, nil
}

func cloneListing(src Listing) Listing {
	return Listing{
		Path:      src.Path,
		ParentKey: src.ParentKey,
		Dirs:      append([]weiyun.DirItem(nil), src.Dirs...),
		Files:     append([]weiyun.FileItem(nil), src.Files...),
	}
}

func (s *Service) listAll(ctx context.Context, dir dirRef) (Listing, error) {
	s.cacheMu.Lock()
	ttl := s.dirTTL
	if ttl <= 0 {
		s.cacheMu.Unlock()
		return s.fetchListAll(ctx, dir)
	}
	now := time.Now()
	if cached, ok := s.dirCache[dir.path]; ok && now.Before(cached.expiresAt) {
		out := cloneListing(cached.listing)
		s.cacheMu.Unlock()
		return out, nil
	}
	if call, ok := s.dirInflight[dir.path]; ok {
		s.cacheMu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return Listing{}, call.err
			}
			return cloneListing(call.listing), nil
		case <-ctx.Done():
			return Listing{}, ctx.Err()
		}
	}
	call := &inflightDir{done: make(chan struct{})}
	s.dirInflight[dir.path] = call
	s.cacheMu.Unlock()

	listing, err := s.fetchListAll(ctx, dir)

	s.cacheMu.Lock()
	delete(s.dirInflight, dir.path)
	if err == nil && s.dirTTL > 0 {
		s.dirCache[dir.path] = cachedDir{listing: cloneListing(listing), expiresAt: time.Now().Add(s.dirTTL)}
	}
	call.listing = listing
	call.err = err
	close(call.done)
	s.cacheMu.Unlock()

	if err != nil {
		return Listing{}, err
	}
	return cloneListing(listing), nil
}

func (s *Service) fetchListAll(ctx context.Context, dir dirRef) (Listing, error) {
	listing := Listing{Path: dir.path}
	seenPages := make(map[string]bool)
	for offset := uint32(0); ; {
		if err := ctx.Err(); err != nil {
			return Listing{}, err
		}
		response, err := s.client.List(ctx, weiyun.ListRequest{Offset: offset, Limit: pageSize, DirKey: dir.dirKey, PDirKey: dir.parentKey})
		if err != nil {
			return Listing{}, err
		}
		if listing.ParentKey == "" {
			listing.ParentKey = response.PDirKey
		} else if response.PDirKey != listing.ParentKey {
			return Listing{}, ErrNoProgress
		}
		count := len(response.DirList) + len(response.FileList)
		if response.FinishFlag {
			listing.Dirs = append(listing.Dirs, response.DirList...)
			listing.Files = append(listing.Files, response.FileList...)
			return listing, nil
		}
		if count == 0 {
			return Listing{}, ErrNoProgress
		}
		signature := pageSignature(response)
		if seenPages[signature] {
			return Listing{}, ErrNoProgress
		}
		seenPages[signature] = true
		listing.Dirs = append(listing.Dirs, response.DirList...)
		listing.Files = append(listing.Files, response.FileList...)
		offset += uint32(count)
	}
}

func uniqueEntry(fullPath, name string, listing Listing) (Entry, error) {
	entry := Entry{Path: fullPath, Name: name, ParentKey: listing.ParentKey}
	matches := 0
	for i := range listing.Dirs {
		if listing.Dirs[i].DirName == name {
			matches++
			item := listing.Dirs[i]
			entry.Dir = &item
		}
	}
	for i := range listing.Files {
		if listing.Files[i].Filename == name {
			matches++
			item := listing.Files[i]
			entry.File = &item
		}
	}
	if matches == 0 {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, fullPath)
	}
	if matches > 1 {
		return Entry{}, fmt.Errorf("%w: %s", ErrAmbiguous, fullPath)
	}
	return entry, nil
}

func pageSignature(response weiyun.ListResponse) string {
	var b strings.Builder
	b.WriteString(response.PDirKey)
	for _, item := range response.DirList {
		b.WriteString("|d:")
		b.WriteString(item.DirKey)
		b.WriteString(":")
		b.WriteString(item.DirName)
	}
	for _, item := range response.FileList {
		b.WriteString("|f:")
		b.WriteString(item.FileID)
		b.WriteString(":")
		b.WriteString(item.Filename)
	}
	return b.String()
}
