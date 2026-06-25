package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/lambda-mega-zipper/pkg/zipasm"
)

type Request struct {
	SourceBucket   string `json:"sourceBucket"`
	SourcePrefix   string `json:"sourcePrefix"`
	OutputBucket   string `json:"outputBucket"`
	OutputKey      string `json:"outputKey"`
	WorkerFunction string `json:"workerFunction"`
}

type Response struct {
	OutputBucket string `json:"outputBucket"`
	OutputKey    string `json:"outputKey"`
	TotalSize    uint64 `json:"totalSize"`
	Duration     string `json:"duration"`
}

func handler(ctx context.Context, req Request) (*Response, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	s3Client := s3.NewFromConfig(cfg)
	lambdaClient := lambdasvc.NewFromConfig(cfg)

	// === PLAN PHASE ===
	// (inline the planner logic — list files, compute layout, create multipart, build assignments)

	// List all objects
	type objInfo struct {
		key  string
		size uint64
	}
	var objects []objInfo
	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
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
		return nil, fmt.Errorf("no objects found")
	}

	// Compute zip layout (same as planner — simplified: all files sequential, no UploadPartCopy for now)
	names := make([]string, len(objects))
	sizes := make([]uint64, len(objects))
	for i, o := range objects {
		names[i] = o.key
		sizes[i] = o.size
	}
	plan := zipasm.Plan(names, sizes)

	// Initiate multipart upload
	mpu, err := s3Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &req.OutputBucket,
		Key:    &req.OutputKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart: %w", err)
	}
	uploadID := *mpu.UploadId

	// Build assignments — one per file, simple streaming (no UploadPartCopy in orchestrator mode)
	var assignments []json.RawMessage
	partNum := int32(1)
	const minPartSize = uint64(5 * 1024 * 1024)
	var batch []map[string]interface{}
	var batchSize uint64

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		a := map[string]interface{}{
			"uploadId":     uploadID,
			"outputBucket": req.OutputBucket,
			"outputKey":    req.OutputKey,
			"sourceBucket": req.SourceBucket,
			"duos": []map[string]interface{}{
				{"streamFiles": batch},
			},
			"partNumber": partNum,
		}
		payload, _ := json.Marshal(a)
		assignments = append(assignments, payload)
		partNum++
		batch = nil
		batchSize = 0
	}

	for _, e := range plan.Entries {
		batch = append(batch, map[string]interface{}{"key": e.Name, "size": e.Size, "offset": e.Offset})
		batchSize += zipasm.LocalFileHeaderSize(e.Name) + e.Size
		if batchSize >= minPartSize {
			flushBatch()
		}
	}
	// If remaining batch is too small, merge into the last assignment
	if batchSize > 0 && batchSize < minPartSize && len(assignments) > 0 {
		// Pop last assignment, decode, merge batch into its streamFiles, re-encode
		var lastA map[string]interface{}
		json.Unmarshal(assignments[len(assignments)-1], &lastA)
		duos := lastA["duos"].([]interface{})
		duo := duos[0].(map[string]interface{})
		existing := duo["streamFiles"].([]interface{})
		for _, b := range batch {
			existing = append(existing, b)
		}
		duo["streamFiles"] = existing
		lastA["duos"] = []interface{}{duo}
		assignments[len(assignments)-1], _ = json.Marshal(lastA)
		batch = nil
	} else {
		flushBatch()
	}

	// === INVOKE WORKERS IN PARALLEL ===
	type workerResult struct {
		Parts []struct {
			PartNumber int32  `json:"partNumber"`
			ETag       string `json:"etag"`
		} `json:"parts"`
		CRC32s []struct {
			Name  string `json:"name"`
			CRC32 uint32 `json:"crc32"`
		} `json:"crc32s"`
	}

	results := make([]workerResult, len(assignments))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 200) // max 200 concurrent invocations
	var firstErr error
	var errOnce sync.Once

	for i, payload := range assignments {
		wg.Add(1)
		go func(idx int, p []byte) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			out, err := lambdaClient.Invoke(ctx, &lambdasvc.InvokeInput{
				FunctionName: &req.WorkerFunction,
				Payload:      p,
				InvocationType: types.InvocationTypeRequestResponse,
			})
			if err != nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("invoke worker %d: %w", idx, err) })
				return
			}
			if out.FunctionError != nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("worker %d error: %s", idx, string(out.Payload)) })
				return
			}
			var r workerResult
			json.Unmarshal(out.Payload, &r)
			results[idx] = r
		}(i, payload)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	// Build CRC32 map from worker responses (no S3 round-trip needed in orchestrator mode)
	crcMap := make(map[string]uint32)
	for _, r := range results {
		for _, c := range r.CRC32s {
			crcMap[c.Name] = c.CRC32
		}
	}

	// Build CD
	entries := make([]zipasm.FileEntry, len(plan.Entries))
	for i, e := range plan.Entries {
		entries[i] = zipasm.FileEntry{Name: e.Name, Size: e.Size, Offset: e.Offset, CRC32: crcMap[e.Name]}
	}
	cd := zipasm.CentralDirectory(entries)
	eocd := zipasm.EOCD(plan.CDOffset, uint64(len(cd)), uint16(len(entries)))
	tail := append(cd, eocd...)

	// Collect all parts
	var allParts []s3types.CompletedPart
	maxPart := int32(0)
	for _, r := range results {
		for _, p := range r.Parts {
			pn := p.PartNumber
			etag := p.ETag
			allParts = append(allParts, s3types.CompletedPart{PartNumber: &pn, ETag: &etag})
			if pn > maxPart {
				maxPart = pn
			}
		}
	}

	// Upload CD
	cdPart := maxPart + 1
	upOut, err := s3Client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: &req.OutputBucket, Key: &req.OutputKey, UploadId: &uploadID,
		PartNumber: &cdPart, Body: bytes.NewReader(tail),
		ContentLength: func() *int64 { l := int64(len(tail)); return &l }(),
	})
	if err != nil {
		return nil, fmt.Errorf("upload CD: %w", err)
	}
	cdEtag := *upOut.ETag
	allParts = append(allParts, s3types.CompletedPart{PartNumber: &cdPart, ETag: &cdEtag})
	sort.Slice(allParts, func(i, j int) bool { return *allParts[i].PartNumber < *allParts[j].PartNumber })

	_, err = s3Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &req.OutputBucket, Key: &req.OutputKey, UploadId: &uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: allParts},
	})
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}

	return &Response{
		OutputBucket: req.OutputBucket,
		OutputKey:    req.OutputKey,
		TotalSize:    plan.CDOffset + uint64(len(tail)),
	}, nil
}

func main() {
	lambda.Start(handler)
}
