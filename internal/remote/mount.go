package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

var ErrVersionConflict = errors.New("remote file version changed")

type Version struct {
	FileID    string
	ParentKey string
	Name      string
	Size      int64
	MTime     int64
}

func (s *Service) PutFile(ctx context.Context, rawPath string, expected *Version, file io.ReaderAt, size int64) (Version, error) {
	if s.uploader == nil {
		return Version{}, errors.New("upload is unavailable")
	}
	normalized, err := Normalize(rawPath)
	if err != nil {
		return Version{}, err
	}
	if normalized == "/" {
		return Version{}, ErrRootOperation
	}
	parent, name, listing, err := s.parentListing(ctx, normalized)
	if err != nil {
		return Version{}, err
	}
	entry, resolveErr := uniqueEntry(normalized, name, listing)
	if expected == nil {
		if resolveErr == nil {
			return Version{}, ErrConflict
		}
		if !errors.Is(resolveErr, ErrNotFound) {
			return Version{}, resolveErr
		}
	} else {
		if resolveErr != nil || entry.File == nil || versionOf(entry) != *expected {
			return Version{}, ErrVersionConflict
		}
	}

	response, err := s.uploader.Upload(ctx, file, size, name, listing.ParentKey)
	if err != nil {
		return Version{}, err
	}
	s.invalidateDir(parent.path)
	if response.Filename != "" && response.Filename != name {
		return Version{}, fmt.Errorf("%w: upstream stored %q instead of %q", ErrConflict, response.Filename, name)
	}
	stored, err := s.Resolve(ctx, "wy:"+path.Clean(normalized))
	if err != nil {
		return Version{}, err
	}
	if stored.File == nil || stored.Name != name || stored.File.FileSize != size {
		return Version{}, ErrVersionConflict
	}
	return versionOf(stored), nil
}

func (s *Service) Trash(ctx context.Context, rawPath string) error {
	return s.Delete(ctx, rawPath, false)
}

func (s *Service) ReadRange(ctx context.Context, version Version, offset int64, dest []byte) error {
	if len(dest) == 0 {
		return nil
	}
	rawURL, cookie, err := s.downloadSource(ctx, version, false)
	if err != nil {
		return err
	}
	err = s.downloader.DownloadRange(ctx, rawURL, cookie, offset, dest)
	var statusErr *DownloadStatusError
	if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden || statusErr.StatusCode == http.StatusGone) {
		rawURL, cookie, err = s.downloadSource(ctx, version, true)
		if err != nil {
			return err
		}
		err = s.downloader.DownloadRange(ctx, rawURL, cookie, offset, dest)
	}
	return err
}

func (s *Service) downloadSource(ctx context.Context, version Version, refresh bool) (string, string, error) {
	key := version.FileID + "\x00" + version.ParentKey
	s.cacheMu.Lock()
	ttl := s.urlTTL
	if ttl <= 0 {
		s.cacheMu.Unlock()
		return s.fetchDownloadSource(ctx, version)
	}
	if refresh {
		delete(s.urlCache, key)
	} else if cached, ok := s.urlCache[key]; ok && time.Now().Before(cached.expiresAt) {
		s.cacheMu.Unlock()
		return cached.url, cached.cookie, nil
	}
	if call, ok := s.urlInflight[key]; ok {
		s.cacheMu.Unlock()
		select {
		case <-call.done:
			return call.url, call.cookie, call.err
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	call := &inflightURL{done: make(chan struct{})}
	s.urlInflight[key] = call
	s.cacheMu.Unlock()

	rawURL, cookie, err := s.fetchDownloadSource(ctx, version)

	s.cacheMu.Lock()
	delete(s.urlInflight, key)
	if err == nil && s.urlTTL > 0 {
		s.urlCache[key] = cachedURL{url: rawURL, cookie: cookie, expiresAt: time.Now().Add(s.urlTTL)}
	}
	call.url = rawURL
	call.cookie = cookie
	call.err = err
	close(call.done)
	s.cacheMu.Unlock()

	return rawURL, cookie, err
}

func (s *Service) fetchDownloadSource(ctx context.Context, version Version) (string, string, error) {
	response, err := s.client.Download(ctx, weiyun.DownloadRequest{
		Items: []weiyun.DownloadFileItem{{FileID: version.FileID, PDirKey: version.ParentKey}},
	})
	if err != nil {
		return "", "", err
	}
	if len(response.Items) != 1 || response.Items[0].FileID != version.FileID {
		return "", "", errors.New("invalid download response")
	}
	return response.Items[0].HTTPSDownloadURL, response.Items[0].Cookie, nil
}

func versionOf(entry Entry) Version {
	return Version{
		FileID: entry.File.FileID, ParentKey: entry.ParentKey, Name: entry.Name,
		Size: entry.File.FileSize, MTime: entry.File.FileMTime,
	}
}
