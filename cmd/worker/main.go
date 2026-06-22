package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/lambda-mega-zipper/pkg/zipasm"
)

type FileRef struct {
	Key    string `json:"key"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
}

type Duo struct {
	StreamFiles []FileRef `json:"streamFiles"`
	CopyFile    *FileRef  `json:"copyFile,omitempty"`
}

type WorkerRequest struct {
	UploadID     string `json:"uploadId"`
	OutputBucket string `json:"outputBucket"`
	OutputKey    string `json:"outputKey"`
	SourceBucket string `json:"sourceBucket"`
	Duos         []Duo  `json:"duos"`
	PartNumber   int32  `json:"partNumber"`
}

type CRC32Entry struct {
	Name  string `json:"name"`
	CRC32 uint32 `json:"crc32"`
}

type PartInfo struct {
	PartNumber int32  `json:"partNumber"`
	ETag       string `json:"etag"`
}

type WorkerResponse struct {
	Parts  []PartInfo   `json:"parts"`
	CRC32s []CRC32Entry `json:"crc32s"`
}

func handler(ctx context.Context, req WorkerRequest) (*WorkerResponse, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg)

	var parts []PartInfo
	var crc32s []CRC32Entry
	nextPart := req.PartNumber

	for _, duo := range req.Duos {
		var buf bytes.Buffer

		// Stream files: download each, write LOC+data, compute CRC
		for _, f := range duo.StreamFiles {
			out, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: &req.SourceBucket,
				Key:    &f.Key,
			})
			if err != nil {
				return nil, fmt.Errorf("get %s: %w", f.Key, err)
			}

			entry := zipasm.FileEntry{Name: f.Key, Size: f.Size}
			headerOffset := buf.Len()
			buf.Write(zipasm.LocalFileHeader(entry))

			crc, _, err := zipasm.CRC32Stream(&buf, out.Body)
			out.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", f.Key, err)
			}

			// Patch CRC32 into local file header
			b := buf.Bytes()
			b[headerOffset+14] = byte(crc)
			b[headerOffset+15] = byte(crc >> 8)
			b[headerOffset+16] = byte(crc >> 16)
			b[headerOffset+17] = byte(crc >> 24)

			crc32s = append(crc32s, CRC32Entry{Name: f.Key, CRC32: crc})
		}

		// If there's a copy file, get its CRC32 via HeadObject and append its LOC
		if duo.CopyFile != nil {
			head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket:       &req.SourceBucket,
				Key:          &duo.CopyFile.Key,
				ChecksumMode: types.ChecksumModeEnabled,
			})
			if err != nil {
				return nil, fmt.Errorf("head %s: %w", duo.CopyFile.Key, err)
			}

			var crc32val uint32
			if head.ChecksumCRC32 != nil {
				crc32val = decodeCRC32(*head.ChecksumCRC32)
			}

			entry := zipasm.FileEntry{Name: duo.CopyFile.Key, Size: duo.CopyFile.Size, CRC32: crc32val}
			buf.Write(zipasm.LocalFileHeader(entry))
			crc32s = append(crc32s, CRC32Entry{Name: duo.CopyFile.Key, CRC32: crc32val})
		}

		// UploadPart: buffer contains LOC+data of stream files + LOC of copy file
		if buf.Len() > 0 {
			pn := nextPart
			body := bytes.NewReader(buf.Bytes())
			upOut, err := client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:        &req.OutputBucket,
				Key:           &req.OutputKey,
				UploadId:      &req.UploadID,
				PartNumber:    &pn,
				Body:          body,
				ContentLength: func() *int64 { l := int64(buf.Len()); return &l }(),
			})
			if err != nil {
				return nil, fmt.Errorf("upload part %d: %w", pn, err)
			}
			parts = append(parts, PartInfo{PartNumber: pn, ETag: *upOut.ETag})
			nextPart++
		}

		// UploadPartCopy: big file data (server-side copy, no download)
		if duo.CopyFile != nil {
			pn := nextPart
			copySource := fmt.Sprintf("%s/%s", req.SourceBucket, duo.CopyFile.Key)
			cpOut, err := client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
				Bucket:     &req.OutputBucket,
				Key:        &req.OutputKey,
				UploadId:   &req.UploadID,
				PartNumber: &pn,
				CopySource: &copySource,
			})
			if err != nil {
				return nil, fmt.Errorf("copy part %s: %w", duo.CopyFile.Key, err)
			}
			parts = append(parts, PartInfo{PartNumber: pn, ETag: *cpOut.CopyPartResult.ETag})
			nextPart++
		}
	}

	return &WorkerResponse{Parts: parts, CRC32s: crc32s}, nil
}

// decodeCRC32 decodes S3's base64-encoded CRC32 to uint32 (big-endian 4 bytes)
func decodeCRC32(s string) uint32 {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 4 {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func main() {
	lambda.Start(handler)
}
