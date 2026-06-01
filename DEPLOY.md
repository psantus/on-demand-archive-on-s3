# Deploy & Test (profile: training, region: eu-west-3)

## 1. Build Lambda binaries

```bash
cd ~/Downloads/lambda-mega-zipper
make build
```

## 2. Bootstrap CDK (first time only)

```bash
cd infra
npx cdk bootstrap --region eu-west-3
```

## 3. Deploy CDK stack

```bash
npx cdk deploy --require-approval never --region eu-west-3
```

## 4. Get the state machine ARN

```bash
STATE_MACHINE_ARN=$(aws stepfunctions list-state-machines \
  --region eu-west-3 \
  --query "stateMachines[?contains(name,'MegaZipper')].stateMachineArn" \
  --output text)
```

## 5. Create a test bucket (if needed)

```bash
BUCKET="mega-zipper-test-$(aws sts get-caller-identity --query Account --output text)"
aws s3 mb "s3://$BUCKET" --region eu-west-3
```

## 6. Upload test files

Option A — Copy NOAA public dataset (~275 CSV files, varying sizes):

```bash
aws s3 sync s3://noaa-ghcn-pds/csv/by_year/ s3://$BUCKET/input/ --region eu-west-3
```

Option B — Generate synthetic files (10 x 1MB):

```bash
for i in $(seq 1 10); do
  dd if=/dev/urandom bs=1m count=1 2>/dev/null | \
    aws s3 cp - "s3://$BUCKET/input/file_$(printf '%04d' $i).bin" \
    --region eu-west-3 --quiet &
  [ $((i % 50)) -eq 0 ] && wait
done
wait
```

## 7. Start execution

```bash
aws stepfunctions start-execution \
  --region eu-west-3 \
  --state-machine-arn "$STATE_MACHINE_ARN" \
  --input "{
    \"sourceBucket\": \"$BUCKET\",
    \"sourcePrefix\": \"input/\",
    \"outputBucket\": \"$BUCKET\",
    \"outputKey\": \"output/archive.zip\",
    \"workerCount\": 10
  }"
```

## 8. Watch execution

```bash
EXEC_ARN=$(aws stepfunctions list-executions \
  --region eu-west-3 \
  --state-machine-arn "$STATE_MACHINE_ARN" \
  --status-filter RUNNING \
  --query 'executions[0].executionArn' --output text)

while true; do
  STATUS=$(aws stepfunctions describe-execution \
    --region eu-west-3 \
    --execution-arn "$EXEC_ARN" \
    --query 'status' --output text)
  echo "Status: $STATUS"
  [ "$STATUS" != "RUNNING" ] && break
  sleep 3
done
```

## 9. Download and verify

```bash
aws s3 cp "s3://$BUCKET/output/archive.zip" /tmp/test-output.zip \
  --region eu-west-3
unzip -t /tmp/test-output.zip
```

## Alternative: use the integration test script

```bash
AWS_DEFAULT_REGION=eu-west-3 \
  ./scripts/integration_test.sh "$BUCKET" "$BUCKET" "$STATE_MACHINE_ARN" 10 1
```
