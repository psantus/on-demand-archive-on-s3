package main

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/lambda-mega-zipper/pkg/zipasm"
)

type FileInfo struct {
	Key    string `json:"key"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
}

type WorkerRequest struct {
	UploadID     string     `json:"uploadId"`
	OutputBucket string     `json:"outputBucket"`
	OutputKey    string     `json:"outputKey"`
	SourceBucket string     `json:"sourceBucket"`
	Files        []FileInfo `json:"files"`
	PartNumber   int32      `json:"partNumber"`
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

const partSizeThreshold = 100 * 1024 * 1024 // 100MB flush threshold
const minPartSize = 5 * 1024 * 1024         // 5MB S3 minimum

func handler(ctx context.Context, req WorkerRequest) (*WorkerResponse, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg)

	var buf bytes.Buffer
	crc32s := make([]CRC32Entry, 0, len(req.Files))
	var parts []PartInfo
	nextPart := req.PartNumber

	flush := func() error {
		if buf.Len() < 5*1024*1024 && len(req.Files) > 0 {
			return nil // S3 min part size is 5MB (except last)
		}
		if buf.Len() == 0 {
			return nil
		}
		body := bytes.NewReader(buf.Bytes())
		pn := nextPart
		upOut, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        &req.OutputBucket,
			Key:           &req.OutputKey,
			UploadId:      &req.UploadID,
			PartNumber:    &pn,
			Body:          body,
			ContentLength: func() *int64 { l := int64(buf.Len()); return &l }(),
		})
		if err != nil {
			return fmt.Errorf("upload part %d: %w", pn, err)
		}
		parts = append(parts, PartInfo{PartNumber: pn, ETag: *upOut.ETag})
		nextPart++
		buf.Reset()
		return nil
	}

	for _, f := range req.Files {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: &req.SourceBucket,
			Key:    &f.Key,
		})
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", f.Key, err)
		}

		entry := zipasm.FileEntry{Name: f.Key, Size: f.Size}
		buf.Write(zipasm.LocalFileHeader(entry))

		crc, _, err := zipasm.CRC32Stream(&buf, out.Body)
		out.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Key, err)
		}
		crc32s = append(crc32s, CRC32Entry{Name: f.Key, CRC32: crc})

		if buf.Len() >= partSizeThreshold {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}

	// Final flush
	if buf.Len() > 0 {
		body := bytes.NewReader(buf.Bytes())
		pn := nextPart
		upOut, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        &req.OutputBucket,
			Key:           &req.OutputKey,
			UploadId:      &req.UploadID,
			PartNumber:    &pn,
			Body:          body,
			ContentLength: func() *int64 { l := int64(buf.Len()); return &l }(),
		})
		if err != nil {
			return nil, fmt.Errorf("upload final part %d: %w", pn, err)
		}
		parts = append(parts, PartInfo{PartNumber: pn, ETag: *upOut.ETag})
	}

	return &WorkerResponse{Parts: parts, CRC32s: crc32s}, nil
}

func main() {
	lambda.Start(handler)
}
