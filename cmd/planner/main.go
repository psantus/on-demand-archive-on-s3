package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/lambda-mega-zipper/pkg/zipasm"
)

const minPartSize = 5 * 1024 * 1024 // 5MB

type PlanRequest struct {
	SourceBucket string `json:"sourceBucket"`
	SourcePrefix string `json:"sourcePrefix"`
	OutputBucket string `json:"outputBucket"`
	OutputKey    string `json:"outputKey"`
}

// A Duo is: one UploadPart (small files + LOC of copied file) followed by one UploadPartCopy (big file data)
type Duo struct {
	// Files to download and include in the UploadPart (small files or one streamed big file)
	StreamFiles []FileRef `json:"streamFiles"`
	// The big file whose LOC goes at the end of the UploadPart, and whose data is UploadPartCopy'd
	CopyFile *FileRef `json:"copyFile,omitempty"`
}

type FileRef struct {
	Key    string `json:"key"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"` // offset in final zip of the LOC
}

type Assignment struct {
	UploadID     string `json:"uploadId"`
	OutputBucket string `json:"outputBucket"`
	OutputKey    string `json:"outputKey"`
	SourceBucket string `json:"sourceBucket"`
	Duos         []Duo  `json:"duos"`
	PartNumber   int32  `json:"partNumber"` // starting part number (each duo uses 2: upload + copy)
}

type PlanResponse struct {
	UploadID    string       `json:"uploadId"`
	Assignments []Assignment `json:"assignments"`
	CDInfo      CDInfo       `json:"cdInfo"`
}

type CDInfo struct {
	Entries  []CDEntry `json:"entries"`
	CDOffset uint64    `json:"cdOffset"`
}

type CDEntry struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
}

func handler(ctx context.Context, req PlanRequest) (*PlanResponse, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg)

	// List all objects
	type objInfo struct {
		key  string
		size uint64
	}
	var objects []objInfo
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: &req.SourceBucket,
		Prefix: &req.SourcePrefix,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, objInfo{key: *obj.Key, size: uint64(*obj.Size)})
		}
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("no objects found at s3://%s/%s", req.SourceBucket, req.SourcePrefix)
	}

	// Separate into big (≥5MB) and small (<5MB)
	var bigs, smalls []objInfo
	for _, o := range objects {
		if o.size >= minPartSize {
			bigs = append(bigs, o)
		} else {
			smalls = append(smalls, o)
		}
	}

	// Sort bigs by size descending (we'll copy the largest, stream the smallest)
	sort.Slice(bigs, func(i, j int) bool { return bigs[i].size > bigs[j].size })

	// Build duos and compute zip offsets
	// Order in zip: for each duo, stream files come first, then LOC of copy file, then copy file data
	var duos []Duo
	var zipEntries []zipasm.FileEntry // in zip order
	var offset uint64

	addEntry := func(key string, size uint64) FileRef {
		ref := FileRef{Key: key, Size: size, Offset: offset}
		zipEntries = append(zipEntries, zipasm.FileEntry{Name: key, Size: size, Offset: offset})
		offset += zipasm.LocalFileHeaderSize(key) + size
		return ref
	}

	// Phase 1: pair small files with the largest big files
	smallIdx := 0
	copyIdx := 0 // index into bigs (largest first)
	for copyIdx < len(bigs) && smallIdx < len(smalls) {
		var partDataSize uint64
		startSmall := smallIdx

		// Accumulate small files until we reach 5MB
		for smallIdx < len(smalls) {
			s := smalls[smallIdx]
			entrySize := zipasm.LocalFileHeaderSize(s.key) + s.size
			if partDataSize+entrySize >= minPartSize {
				break
			}
			partDataSize += entrySize
			smallIdx++
		}
		for smallIdx < len(smalls) && partDataSize < minPartSize {
			s := smalls[smallIdx]
			partDataSize += zipasm.LocalFileHeaderSize(s.key) + s.size
			smallIdx++
		}

		// Check if partDataSize + LOC of copy file reaches 5MB
		big := bigs[copyIdx]
		totalPartSize := partDataSize + zipasm.LocalFileHeaderSize(big.key)
		if totalPartSize < minPartSize {
			// Not enough data — revert smalls, they'll go to "remaining smalls"
			smallIdx = startSmall
			break
		}

		// Commit: add entries to zip layout
		var streamFiles []FileRef
		for i := startSmall; i < smallIdx; i++ {
			s := smalls[i]
			ref := addEntry(s.key, s.size)
			streamFiles = append(streamFiles, ref)
		}

		copyRef := addEntry(big.key, big.size)

		duos = append(duos, Duo{
			StreamFiles: streamFiles,
			CopyFile:    &copyRef,
		})
		copyIdx++
	}

	// Collect remaining small files — they'll be prepended to the first Phase 2 duo
	var remainingSmalls []FileRef
	for smallIdx < len(smalls) {
		s := smalls[smallIdx]
		ref := addEntry(s.key, s.size)
		remainingSmalls = append(remainingSmalls, ref)
		smallIdx++
	}

	// Phase 2: when out of small files, pair remaining bigs:
	// stream smallest remaining big + LOC of largest remaining big → UploadPartCopy largest
	firstPhase2 := true
	for copyIdx < len(bigs) {
		remaining := len(bigs) - copyIdx
		if remaining == 1 {
			// Last big file: just stream it (prepend remaining smalls if any)
			big := bigs[copyIdx]
			ref := addEntry(big.key, big.size)
			streamFiles := append(remainingSmalls, ref)
			remainingSmalls = nil
			duos = append(duos, Duo{StreamFiles: streamFiles})
			copyIdx++
		} else {
			// Pair: stream smallest remaining (end of slice), copy largest remaining (copyIdx)
			streamBig := bigs[len(bigs)-1]
			bigs = bigs[:len(bigs)-1] // pop from end

			streamRef := addEntry(streamBig.key, streamBig.size)
			copyBig := bigs[copyIdx]
			copyRef := addEntry(copyBig.key, copyBig.size)

			streamFiles := []FileRef{streamRef}
			if firstPhase2 {
				streamFiles = append(remainingSmalls, streamRef)
				remainingSmalls = nil
				firstPhase2 = false
			}
			duos = append(duos, Duo{
				StreamFiles: streamFiles,
				CopyFile:    &copyRef,
			})
			copyIdx++
		}
	}

	// If there are still remaining smalls (no bigs at all), add as standalone
	if len(remainingSmalls) > 0 {
		duos = append(duos, Duo{StreamFiles: remainingSmalls})
	}

	// Compute zip plan for CD
	cdOffset := offset
	plan := &zipasm.ZipPlan{
		Entries:  zipEntries,
		CDOffset: cdOffset,
	}
	_ = plan

	// Initiate multipart upload
	mpu, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &req.OutputBucket,
		Key:    &req.OutputKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart upload: %w", err)
	}
	uploadID := *mpu.UploadId

	// One assignment per duo
	var assignments []Assignment
	partNumberCursor := int32(1)
	for _, d := range duos {
		assignments = append(assignments, Assignment{
			UploadID:     uploadID,
			OutputBucket: req.OutputBucket,
			OutputKey:    req.OutputKey,
			SourceBucket: req.SourceBucket,
			Duos:         []Duo{d},
			PartNumber:   partNumberCursor,
		})
		partNumberCursor++
		if d.CopyFile != nil {
			partNumberCursor++
		}
	}

	// Build CD info
	cdEntries := make([]CDEntry, len(zipEntries))
	for i, e := range zipEntries {
		cdEntries[i] = CDEntry{Name: e.Name, Size: e.Size, Offset: e.Offset}
	}

	return &PlanResponse{
		UploadID:    uploadID,
		Assignments: assignments,
		CDInfo:      CDInfo{Entries: cdEntries, CDOffset: cdOffset},
	}, nil
}

func main() {
	lambda.Start(handler)
}
