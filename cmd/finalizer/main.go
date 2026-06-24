package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/lambda-mega-zipper/pkg/zipasm"
)

type CDEntry struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
}

type CDInfo struct {
	Entries  []CDEntry `json:"entries"`
	CDOffset uint64    `json:"cdOffset"`
}

type CRC32Entry struct {
	Name  string `json:"name"`
	CRC32 uint32 `json:"crc32"`
}

type PartInfo struct {
	PartNumber int32  `json:"partNumber"`
	ETag       string `json:"etag"`
}

type WorkerResult struct {
	Parts []PartInfo `json:"parts"`
}

type FinalizeRequest struct {
	UploadID      string         `json:"uploadId"`
	OutputBucket  string         `json:"outputBucket"`
	OutputKey     string         `json:"outputKey"`
	CDInfoBucket  string         `json:"cdInfoBucket"`
	CDInfoKey     string         `json:"cdInfoKey"`
	WorkerResults []WorkerResult `json:"workerResults"`
}

type FinalizeResponse struct {
	OutputBucket string `json:"outputBucket"`
	OutputKey    string `json:"outputKey"`
	TotalSize    uint64 `json:"totalSize"`
}

func handler(ctx context.Context, req FinalizeRequest) (*FinalizeResponse, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg)

	// Read CDInfo from S3
	cdObj, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &req.CDInfoBucket,
		Key:    &req.CDInfoKey,
	})
	if err != nil {
		return nil, fmt.Errorf("get cdinfo: %w", err)
	}
	cdInfoBytes, _ := io.ReadAll(cdObj.Body)
	cdObj.Body.Close()

	var cdInfo CDInfo
	if err := json.Unmarshal(cdInfoBytes, &cdInfo); err != nil {
		return nil, fmt.Errorf("parse cdinfo: %w", err)
	}

	// Read CRC32s from S3 (written by workers)
	crcMap := make(map[string]uint32)
	crcPrefix := "_plan/crc32s/"
	crcPaginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: &req.OutputBucket,
		Prefix: &crcPrefix,
	})
	for crcPaginator.HasMorePages() {
		page, err := crcPaginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list crc32s: %w", err)
		}
		for _, obj := range page.Contents {
			crcObj, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: &req.OutputBucket,
				Key:    obj.Key,
			})
			if err != nil {
				continue
			}
			crcBytes, _ := io.ReadAll(crcObj.Body)
			crcObj.Body.Close()
			var entries []CRC32Entry
			json.Unmarshal(crcBytes, &entries)
			for _, e := range entries {
				crcMap[e.Name] = e.CRC32
			}
		}
	}

	// Build entries with real CRC32 values
	entries := make([]zipasm.FileEntry, len(cdInfo.Entries))
	for i, e := range cdInfo.Entries {
		entries[i] = zipasm.FileEntry{
			Name:   e.Name,
			Size:   e.Size,
			Offset: e.Offset,
			CRC32:  crcMap[e.Name],
		}
	}

	// Build central directory + EOCD
	cd := zipasm.CentralDirectory(entries)
	eocd := zipasm.EOCD(cdInfo.CDOffset, uint64(len(cd)), uint16(len(entries)))
	tail := append(cd, eocd...)

	// Collect all parts from workers
	var allParts []types.CompletedPart
	maxPart := int32(0)
	for _, wr := range req.WorkerResults {
		for _, p := range wr.Parts {
			pn := p.PartNumber
			etag := p.ETag
			allParts = append(allParts, types.CompletedPart{PartNumber: &pn, ETag: &etag})
			if pn > maxPart {
				maxPart = pn
			}
		}
	}

	// Upload CD as final part
	cdPartNumber := maxPart + 1
	body := bytes.NewReader(tail)
	upOut, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        &req.OutputBucket,
		Key:           &req.OutputKey,
		UploadId:      &req.UploadID,
		PartNumber:    &cdPartNumber,
		Body:          body,
		ContentLength: func() *int64 { l := int64(len(tail)); return &l }(),
	})
	if err != nil {
		return nil, fmt.Errorf("upload CD part: %w", err)
	}

	cdEtag := *upOut.ETag
	allParts = append(allParts, types.CompletedPart{PartNumber: &cdPartNumber, ETag: &cdEtag})
	sort.Slice(allParts, func(i, j int) bool { return *allParts[i].PartNumber < *allParts[j].PartNumber })

	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   &req.OutputBucket,
		Key:      &req.OutputKey,
		UploadId: &req.UploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: allParts},
	})
	if err != nil {
		return nil, fmt.Errorf("complete multipart: %w", err)
	}

	return &FinalizeResponse{
		OutputBucket: req.OutputBucket,
		OutputKey:    req.OutputKey,
		TotalSize:    cdInfo.CDOffset + uint64(len(tail)),
	}, nil
}

func main() {
	lambda.Start(handler)
}
