package upload

import (
	"context"
	"encoding/base64"
	"errors"
	"io"

	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
)

const (
	maxChunkSize  = 16 * 1024 * 1024
	maxNoProgress = 8
)

var (
	ErrInvalidChannel = errors.New("invalid upload channel")
	ErrNoProgress     = errors.New("upload made no progress")
)

type caller interface {
	Upload(context.Context, weiyun.UploadRequest) (weiyun.UploadResponse, error)
}

type Uploader struct {
	client caller
}

func New(client caller) *Uploader {
	return &Uploader{client: client}
}

func (u *Uploader) Upload(ctx context.Context, file io.ReaderAt, size int64, filename, pdirKey string) (weiyun.UploadResponse, error) {
	hashes, err := Calculate(ctx, file, size)
	if err != nil {
		return weiyun.UploadResponse{}, err
	}
	preflight := weiyun.UploadRequest{
		Filename: filename, FileSize: uint64(size), FileSHA: hashes.FileSHA, FileMD5: hashes.FileMD5,
		BlockSHAList: hashes.BlockSHAList, CheckSHA: hashes.CheckSHA, CheckData: hashes.CheckData, PDirKey: pdirKey,
	}

	noProgress := 0
	seenChannels := make(map[weiyun.ChannelInfo]bool)
	for {
		if err := ctx.Err(); err != nil {
			return weiyun.UploadResponse{}, err
		}
		response, err := u.client.Upload(ctx, preflight)
		if err != nil {
			return weiyun.UploadResponse{}, err
		}
		if response.FileExist || response.UploadState == 2 {
			return response, nil
		}

		var channel *weiyun.ChannelInfo
		for i := range response.ChannelList {
			if response.ChannelList[i].Len > 0 {
				channel = &response.ChannelList[i]
				break
			}
		}
		if channel == nil {
			noProgress++
			if noProgress >= maxNoProgress {
				return weiyun.UploadResponse{}, ErrNoProgress
			}
			continue
		}
		if channel.Len > maxChunkSize || channel.Offset > uint64(size) || uint64(channel.Len) > uint64(size)-channel.Offset {
			return weiyun.UploadResponse{}, ErrInvalidChannel
		}
		if seenChannels[*channel] {
			noProgress++
			if noProgress >= maxNoProgress {
				return weiyun.UploadResponse{}, ErrNoProgress
			}
		} else {
			seenChannels[*channel] = true
			noProgress = 0
		}

		chunk := make([]byte, channel.Len)
		read, err := file.ReadAt(chunk, int64(channel.Offset))
		if err != nil && !errors.Is(err, io.EOF) {
			return weiyun.UploadResponse{}, err
		}
		if read != len(chunk) {
			return weiyun.UploadResponse{}, io.ErrUnexpectedEOF
		}
		response, err = u.client.Upload(ctx, weiyun.UploadRequest{
			Filename: filename, FileSize: uint64(size), FileSHA: hashes.FileSHA,
			BlockSHAList: []string{}, CheckSHA: hashes.CheckSHA,
			UploadKey: response.UploadKey, ChannelList: response.ChannelList, ChannelID: channel.ID,
			Ex: response.Ex, FileData: base64.StdEncoding.EncodeToString(chunk),
		})
		if err != nil {
			if weiyun.IsTemporary(err) {
				continue
			}
			return weiyun.UploadResponse{}, err
		}
		if response.UploadState == 2 {
			return response, nil
		}
	}
}
