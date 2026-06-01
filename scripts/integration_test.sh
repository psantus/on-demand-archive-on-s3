#!/usr/bin/env bash
set -euo pipefail

# Integration test for lambda-mega-zipper
# Usage: ./scripts/integration_test.sh <source-bucket> <output-bucket> <state-machine-arn> [file-count] [file-size-mb]

SOURCE_BUCKET="${1:?Usage: $0 <source-bucket> <output-bucket> <state-machine-arn> [file-count] [file-size-mb]}"
OUTPUT_BUCKET="${2:?}"
STATE_MACHINE_ARN="${3:?}"
FILE_COUNT="${4:-10}"
FILE_SIZE_MB="${5:-1}"
PREFIX="test-input/"
OUTPUT_KEY="test-output/result.zip"

echo "=== Lambda Mega Zipper Integration Test ==="
echo "Files: ${FILE_COUNT} x ${FILE_SIZE_MB}MB"
echo "Source: s3://${SOURCE_BUCKET}/${PREFIX}"
echo "Output: s3://${OUTPUT_BUCKET}/${OUTPUT_KEY}"

# Step 1: Upload test files
echo -e "\n[1/5] Uploading ${FILE_COUNT} test files..."
for i in $(seq 1 "$FILE_COUNT"); do
  dd if=/dev/urandom bs=1m count="$FILE_SIZE_MB" 2>/dev/null | \
    aws s3 cp - "s3://${SOURCE_BUCKET}/${PREFIX}file_$(printf '%04d' $i).bin" --quiet &
done
wait
echo "Done."

# Step 2: Start execution
echo -e "\n[2/5] Starting Step Functions execution..."
INPUT=$(cat <<EOF
{
  "sourceBucket": "${SOURCE_BUCKET}",
  "sourcePrefix": "${PREFIX}",
  "outputBucket": "${OUTPUT_BUCKET}",
  "outputKey": "${OUTPUT_KEY}",
  "workerCount": ${FILE_COUNT}
}
EOF
)
START_TIME=$(date +%s)
EXEC_ARN=$(aws stepfunctions start-execution \
  --state-machine-arn "$STATE_MACHINE_ARN" \
  --input "$INPUT" \
  --query 'executionArn' --output text)
echo "Execution: $EXEC_ARN"

# Step 3: Wait for completion
echo -e "\n[3/5] Waiting for completion..."
while true; do
  STATUS=$(aws stepfunctions describe-execution \
    --execution-arn "$EXEC_ARN" \
    --query 'status' --output text)
  if [ "$STATUS" != "RUNNING" ]; then
    break
  fi
  sleep 2
done
END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

if [ "$STATUS" != "SUCCEEDED" ]; then
  echo "FAILED! Status: $STATUS"
  aws stepfunctions describe-execution --execution-arn "$EXEC_ARN" --query 'error'
  exit 1
fi
echo "Completed in ${ELAPSED}s"

# Step 4: Download and verify
echo -e "\n[4/5] Downloading and verifying zip..."
TMPZIP=$(mktemp /tmp/mega-zipper-test.XXXXXX.zip)
aws s3 cp "s3://${OUTPUT_BUCKET}/${OUTPUT_KEY}" "$TMPZIP" --quiet
unzip -t "$TMPZIP" | tail -3

# Step 5: Report
echo -e "\n[5/5] Results:"
ZIP_SIZE=$(stat -f%z "$TMPZIP" 2>/dev/null || stat -c%s "$TMPZIP")
echo "  Wall-clock time: ${ELAPSED}s"
echo "  Zip size: $(echo "scale=2; $ZIP_SIZE/1048576" | bc)MB"
echo "  Files in zip: $(unzip -l "$TMPZIP" | tail -1 | awk '{print $2}')"
echo "  Throughput: $(echo "scale=2; $ZIP_SIZE/$ELAPSED/1048576" | bc)MB/s"

# Cleanup
rm -f "$TMPZIP"
echo -e "\n=== PASS ==="
