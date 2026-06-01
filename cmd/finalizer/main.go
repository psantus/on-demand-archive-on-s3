package main

import (
	"bytes"
	"context"
	"fmt"
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
	Parts  []PartInfo   `json:"parts"`
	CRC32s []CRC32Entry `json:"crc32s"`
}

type FinalizeRequest struct {
	UploadID      string         `json:"uploadId"`
	OutputBucket  string         `json:"outputBucket"`
	OutputKey     string         `json:"outputKey"`
	WorkerResults []WorkerResult `json:"workerResults"`
	CDInfo        CDInfo         `json:"cdInfo"`
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

	// Build CRC32 lookup
	crcMap := make(map[string]uint32)
	for _, wr := range req.WorkerResults {
		for _, c := range wr.CRC32s {
			crcMap[c.Name] = c.CRC32
		}
	}

	// Build entries with real CRC32 values
	entries := make([]zipasm.FileEntry, len(req.CDInfo.Entries))
	for i, e := range req.CDInfo.Entries {
		entries[i] = zipasm.FileEntry{
			Name:   e.Name,
			Size:   e.Size,
			Offset: e.Offset,
			CRC32:  crcMap[e.Name],
		}
	}

	// Build central directory + EOCD
	cd := zipasm.CentralDirectory(entries)
	eocd := zipasm.EOCD(req.CDInfo.CDOffset, uint64(len(cd)), uint16(len(entries)))
	tail := append(cd, eocd...)

	// Collect all parts from workers, find max part number
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

	// Upload CD as next part
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
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: allParts,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("complete multipart: %w", err)
	}

	return &FinalizeResponse{
		OutputBucket: req.OutputBucket,
		OutputKey:    req.OutputKey,
		TotalSize:    req.CDInfo.CDOffset + uint64(len(tail)),
	}, nil
}

func main() {
	lambda.Start(handler)
}
