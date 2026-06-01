package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/lambda-mega-zipper/pkg/zipasm"
)

type PlanRequest struct {
	SourceBucket string `json:"sourceBucket"`
	SourcePrefix string `json:"sourcePrefix"`
	OutputBucket string `json:"outputBucket"`
	OutputKey    string `json:"outputKey"`
	WorkerCount  int    `json:"workerCount"`
}

type FileInfo struct {
	Key    string `json:"key"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
}

type Assignment struct {
	UploadID     string     `json:"uploadId"`
	OutputBucket string     `json:"outputBucket"`
	OutputKey    string     `json:"outputKey"`
	SourceBucket string     `json:"sourceBucket"`
	Files        []FileInfo `json:"files"`
	PartNumber   int32      `json:"partNumber"`
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
	var names []string
	var sizes []uint64
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
			names = append(names, *obj.Key)
			sizes = append(sizes, uint64(*obj.Size))
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no objects found at s3://%s/%s", req.SourceBucket, req.SourcePrefix)
	}

	// Compute zip layout
	plan := zipasm.Plan(names, sizes)

	// Initiate multipart upload
	mpu, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &req.OutputBucket,
		Key:    &req.OutputKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart upload: %w", err)
	}
	uploadID := *mpu.UploadId

	// Divide into assignments — balanced by total data size per worker
	n := req.WorkerCount
	if n <= 0 {
		n = 100
	}
	total := len(plan.Entries)

	// Compute total data size (including headers)
	var totalDataSize uint64
	for _, e := range plan.Entries {
		totalDataSize += zipasm.LocalFileHeaderSize(e.Name) + e.Size
	}
	targetPerWorker := totalDataSize / uint64(n)
	if targetPerWorker < 5*1024*1024 {
		targetPerWorker = 5 * 1024 * 1024
	}

	var assignments []Assignment
	var currentFiles []FileInfo
	var currentSize uint64

	for i, e := range plan.Entries {
		currentFiles = append(currentFiles, FileInfo{Key: e.Name, Size: e.Size, Offset: e.Offset})
		currentSize += e.Size + zipasm.LocalFileHeaderSize(e.Name)

		isLast := i == total-1
		reachedTarget := currentSize >= targetPerWorker

		if reachedTarget || isLast {
			assignments = append(assignments, Assignment{
				UploadID:     uploadID,
				OutputBucket: req.OutputBucket,
				OutputKey:    req.OutputKey,
				SourceBucket: req.SourceBucket,
				Files:        currentFiles,
				PartNumber:   int32(len(assignments)*100 + 1),
			})
			currentFiles = nil
			currentSize = 0
		}
	}

	// Build CD info for finalizer
	cdEntries := make([]CDEntry, total)
	for i, e := range plan.Entries {
		cdEntries[i] = CDEntry{Name: e.Name, Size: e.Size, Offset: e.Offset}
	}

	return &PlanResponse{
		UploadID:    uploadID,
		Assignments: assignments,
		CDInfo:      CDInfo{Entries: cdEntries, CDOffset: plan.CDOffset},
	}, nil
}

func main() {
	lambda.Start(handler)
}
