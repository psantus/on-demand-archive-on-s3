# On-Demand Archive on S3

Parallel zip assembly on AWS Lambda. Creates a single ZIP64 archive from thousands of S3 objects in seconds using `UploadPartCopy` and concurrent Lambda workers.

**Benchmark: 3000 × 5MB files (15GB) → single zip in 6 seconds.**

## Architecture

Two modes: **Orchestrator** (fastest, recommended) and **Step Functions** (observable, retryable).

### Orchestrator Lambda (6s for 15GB)

```
┌─────────────────────────────────────────────────────────────┐
│                    Orchestrator Lambda                        │
│                                                              │
│  1. List files, compute zip layout                           │
│  2. CreateMultipartUpload                                    │
│  3. Invoke N workers in parallel (goroutines + Lambda SDK)   │
│  4. Collect parts, read CRC32s from S3                       │
│  5. Build central directory, CompleteMultipartUpload          │
└──────┬──────────────────────────────────────────────┬────────┘
       │ Invoke (sync)                                │
       ▼                                              ▼
┌──────────────┐                              ┌──────────────┐
│  Worker 1    │  ...                         │  Worker N    │
│  UploadPart  │                              │  UploadPart  │
│  + PartCopy  │                              │  + PartCopy  │
└──────────────┘                              └──────────────┘
```

### Step Functions (41s for 15GB)

```
Planner → Distributed Map (N Workers) → Finalizer
```

Used when you need observability, retries, or audit trails.

## How it works

1. **Plan**: List source files, compute deterministic ZIP64 byte offsets (STORE mode), group into "Duos"
2. **Workers**: For each Duo:
   - Stream small files (<5MB): download → write LOC + data → `UploadPart`
   - Big files (≥5MB): `HeadObject` for CRC32, write LOC → `UploadPartCopy` (server-side, no download)
3. **Finalize**: Build central directory with CRC32s, upload as final part, `CompleteMultipartUpload`

**Key insight**: `UploadPartCopy` tells S3 to copy file data directly into the multipart upload without transiting through Lambda. Workers use ~85MB memory regardless of file sizes.

## Prerequisites

- Go 1.21+
- Node.js 18+ (for CDK)
- AWS CLI configured
- Lambda concurrency ≥100 in target region

## Build

```bash
make build
```

## Deploy

```bash
cd infra && npm install --legacy-peer-deps
make deploy
```

## Usage

### Orchestrator (recommended)

```bash
aws lambda invoke --function-name <ORCHESTRATOR_FUNCTION> \
  --cli-binary-format raw-in-base64-out \
  --payload '{
    "sourceBucket": "my-bucket",
    "sourcePrefix": "photos/",
    "outputBucket": "my-bucket",
    "outputKey": "archives/photos.zip",
    "workerFunction": "<WORKER_FUNCTION_NAME>"
  }' result.json
```

### Step Functions

```bash
aws stepfunctions start-execution \
  --state-machine-arn <STATE_MACHINE_ARN> \
  --input '{
    "sourceBucket": "my-bucket",
    "sourcePrefix": "photos/",
    "outputBucket": "my-bucket",
    "outputKey": "archives/photos.zip"
  }'
```

## Performance

Tested with files following a normal distribution (mean=5MB, stddev=1MB):

| Approach | 3000 files (15GB) | 15000 files (73GB) |
|----------|-------------------|--------------------|
| Single Lambda, Rust streaming (Jérémie Gen1) | 212s | — |
| Single Lambda, Rust + UploadPartCopy (Gen2) | 106s | — |
| Step Functions + Distributed Map | 41s | — |
| **Orchestrator Lambda** | **6s** | **20s** |

Worker stats (orchestrator mode, 3000 files):
- Max memory: 85 MB (allocated 3008MB)
- Average duration: 516ms
- Max duration: 1035ms

## Project Structure

```
├── cmd/
│   ├── orchestrator/  # All-in-one: plan + invoke workers + finalize
│   ├── planner/       # Plan phase (Step Functions mode)
│   ├── worker/        # Worker: UploadPart + UploadPartCopy
│   └── finalizer/     # Finalize (Step Functions mode)
├── pkg/
│   └── zipasm/        # ZIP64 STORE mode offset calculator & header builder
├── infra/             # CDK stack (TypeScript)
├── scripts/           # Integration test
└── Makefile
```

## Credits

Inspired by [Jérémie Rodon's article](https://rustysl.com/en/blog/s3-on-demand-archive) and [Fitz's UploadPartCopy idea](https://github.com/FigmentEngine/demo-s3-archiving).
