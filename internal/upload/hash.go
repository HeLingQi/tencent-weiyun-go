package upload

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
)

const (
	blockSize          int64 = 512 * 1024
	hashReadBufferSize       = 64 * 1024
)

var (
	ErrEmptyFileUnsupported = errors.New("empty file upload is not supported by the documented protocol")
	ErrInvalidSize          = errors.New("invalid file size")
)

type Hashes struct {
	FileMD5      string
	FileSHA      string
	CheckSHA     string
	CheckData    string
	BlockSHAList []string
}

func Calculate(ctx context.Context, file io.ReaderAt, size int64) (Hashes, error) {
	if size < 0 {
		return Hashes{}, ErrInvalidSize
	}
	if size == 0 {
		return Hashes{}, ErrEmptyFileUnsupported
	}

	lastBlockSize := size % blockSize
	if lastBlockSize == 0 {
		lastBlockSize = blockSize
	}
	checkBlockSize := lastBlockSize % 128
	if checkBlockSize == 0 {
		checkBlockSize = 128
	}
	beforeLast := size - lastBlockSize
	beforeCheck := size - checkBlockSize

	sha := sha1.New()
	md5Hash := md5.New()
	buffer := make([]byte, hashReadBufferSize)
	result := Hashes{BlockSHAList: make([]string, 0, (size+blockSize-1)/blockSize)}

	if beforeCheck == 0 {
		state, err := shaStateLittleEndian(sha)
		if err != nil {
			return Hashes{}, err
		}
		result.CheckSHA = state
	}

	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return Hashes{}, err
		}
		n := int64(len(buffer))
		if remaining := size - offset; n > remaining {
			n = remaining
		}
		if beforeCheck > offset && n > beforeCheck-offset {
			n = beforeCheck - offset
		}
		if nextBlock := (offset/blockSize + 1) * blockSize; nextBlock <= beforeLast && n > nextBlock-offset {
			n = nextBlock - offset
		}
		read, err := file.ReadAt(buffer[:n], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return Hashes{}, err
		}
		if int64(read) != n {
			return Hashes{}, io.ErrUnexpectedEOF
		}
		chunk := buffer[:read]
		_, _ = sha.Write(chunk)
		_, _ = md5Hash.Write(chunk)
		offset += int64(read)

		if offset <= beforeLast && offset%blockSize == 0 {
			state, err := shaStateLittleEndian(sha)
			if err != nil {
				return Hashes{}, err
			}
			result.BlockSHAList = append(result.BlockSHAList, state)
		}
		if offset == beforeCheck {
			state, err := shaStateLittleEndian(sha)
			if err != nil {
				return Hashes{}, err
			}
			result.CheckSHA = state
		}
	}

	checkData := make([]byte, checkBlockSize)
	read, err := file.ReadAt(checkData, size-checkBlockSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return Hashes{}, err
	}
	if int64(read) != checkBlockSize {
		return Hashes{}, io.ErrUnexpectedEOF
	}
	result.FileMD5 = hex.EncodeToString(md5Hash.Sum(nil))
	result.FileSHA = hex.EncodeToString(sha.Sum(nil))
	result.CheckData = base64.StdEncoding.EncodeToString(checkData)
	result.BlockSHAList = append(result.BlockSHAList, result.FileSHA)
	return result, nil
}

type binaryMarshaler interface {
	MarshalBinary() ([]byte, error)
}

func shaStateLittleEndian(hash io.Writer) (string, error) {
	state, err := hash.(binaryMarshaler).MarshalBinary()
	if err != nil {
		return "", err
	}
	// crypto/sha1's stable binary form starts with four magic bytes, then h0-h4 in big endian.
	if len(state) < 24 {
		return "", errors.New("invalid sha1 state")
	}
	little := make([]byte, 20)
	for i := 0; i < 5; i++ {
		binary.LittleEndian.PutUint32(little[i*4:], binary.BigEndian.Uint32(state[4+i*4:]))
	}
	return hex.EncodeToString(little), nil
}
