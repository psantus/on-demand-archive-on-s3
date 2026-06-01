# Lambda Mega Zipper

Parallel zip assembly on AWS Lambda using Step Functions fan-out. Creates a single zip archive from thousands of S3 objects in seconds by distributing work across concurrent Lambda workers.

## Architecture

```
┌─────────────┐     ┌──────────────────────────────────────┐     ┌───────────────┐
│   Planner   │────▶│   Step Functions Distributed Map     │────▶│   Finalizer   │
│   Lambda    │     │   (N concurrent Worker Lambdas)      │     │   Lambda      │
└─────────────┘     └──────────────────────────────────────┘     └───────────────┘
       │                          │ │ │                                   │
       │ CreateMultipartUpload    │ │ │ UploadPart (parallel)            │ UploadPart (CD)
       ▼                          ▼ ▼ ▼                                   │ CompleteMultipartUpload
┌──────────────────────────────────────────────────────────────────────────▼──┐
│                              S3 Output Bucket                               │
└─────────────────────────────────────────────────────────────────────────────┘
```

**How it works:**

1. **Planner** lists all source files, computes deterministic zip byte offsets (STORE mode = no compression), initiates an S3 multipart upload, and divides files into N worker batches.

2. **Workers** (up to 1000 concurrent) each download their assigned files, construct zip local file headers + raw data, compute CRC32 on the fly, and upload their chunk as an S3 multipart part.

3. **Finalizer** collects CRC32 values from all workers, builds the central directory + EOCD record, uploads it as the final part, and completes the multipart upload.

The key insight: zip STORE mode has deterministic entry offsets (30 + len(filename) + filesize per entry), so workers can write their portions independently without coordination.

## Prerequisites

- Go 1.21+
- Node.js 18+ (for CDK)
- AWS CLI configured
- AWS CDK CLI (`npm install -g aws-cdk`)

## Build

```bash
make build
```

This cross-compiles all three Lambda binaries for `linux/arm64` and packages them as zip files in `infra/lambda/`.

## Deploy

```bash
cd infra && npm install --legacy-peer-deps
make deploy
```

## Usage

Start an execution via AWS CLI:

```bash
aws stepfunctions start-execution \
  --state-machine-arn <STATE_MACHINE_ARN> \
  --input '{
    "sourceBucket": "my-source-bucket",
    "sourcePrefix": "path/to/files/",
    "outputBucket": "my-output-bucket",
    "outputKey": "output/archive.zip",
    "workerCount": 100
  }'
```

## Integration Test

```bash
./scripts/integration_test.sh <source-bucket> <output-bucket> <state-machine-arn> [file-count] [file-size-mb]
```

Example with 10 × 1MB files:
```bash
./scripts/integration_test.sh my-bucket my-bucket arn:aws:states:us-east-1:123456789:stateMachine:MegaZipperSM 10 1
```

## Tuning Parameters

| Parameter | Default | Notes |
|-----------|---------|-------|
| `workerCount` | 100 | Number of parallel workers. More = faster but hits Lambda concurrency limits |
| Worker memory | 3008 MB | Higher = more CPU + network bandwidth (2 vCPUs at 3008MB) |
| Worker timeout | 5 min | Each worker handles ~150MB at default settings |
| Map MaxConcurrency | 1000 | Step Functions limit per Map state |
| Download concurrency | 8 | Goroutines per worker for S3 GETs |

## Performance Estimates

For 3000 × 5MB files (15GB total):

| Workers | Data/Worker | Expected Wall-Clock |
|---------|-------------|---------------------|
| 100 | ~150MB (30 files) | ~20-30s |
| 200 | ~75MB (15 files) | ~12-18s |
| 500 | ~30MB (6 files) | ~8-12s |

Bottlenecks: Lambda cold starts (~200ms for Go on ARM64), S3 GET throughput per worker, Step Functions Map state overhead.

## Project Structure

```
├── cmd/
│   ├── planner/     # Plan phase Lambda
│   ├── worker/      # Worker Lambda (fan-out)
│   └── finalizer/   # Finalize phase Lambda
├── pkg/
│   └── zipasm/      # Zip STORE mode offset calculator & header builder
├── infra/           # CDK stack (TypeScript)
├── scripts/         # Integration test
└── Makefile
```
