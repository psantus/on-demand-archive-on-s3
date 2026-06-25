import * as cdk from 'aws-cdk-lib';
import * as lambda from 'aws-cdk-lib/aws-lambda';
import * as sfn from 'aws-cdk-lib/aws-stepfunctions';
import * as tasks from 'aws-cdk-lib/aws-stepfunctions-tasks';
import * as iam from 'aws-cdk-lib/aws-iam';
import { Construct } from 'constructs';
import * as path from 'path';

export class MegaZipperStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props?: cdk.StackProps) {
    super(scope, id, props);

    const lambdaDir = path.join(__dirname, '..', 'lambda');

    const s3Policy = new iam.PolicyStatement({
      actions: ['s3:GetObject', 's3:PutObject', 's3:ListBucket',
                's3:CreateMultipartUpload', 's3:UploadPart', 's3:UploadPartCopy',
                's3:CompleteMultipartUpload', 's3:AbortMultipartUpload'],
      resources: ['*'],
    });

    const planner = new lambda.Function(this, 'Planner', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'planner.zip')),
      memorySize: 1024,
      timeout: cdk.Duration.minutes(2),
    });
    planner.addToRolePolicy(s3Policy);

    const worker = new lambda.Function(this, 'Worker', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'worker.zip')),
      memorySize: 3008,
      timeout: cdk.Duration.minutes(10),
    });
    worker.addToRolePolicy(s3Policy);

    const finalizer = new lambda.Function(this, 'Finalizer', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'finalizer.zip')),
      memorySize: 2048,
      timeout: cdk.Duration.minutes(5),
    });
    finalizer.addToRolePolicy(s3Policy);

    // Orchestrator Lambda (standalone, no Step Functions needed)
    const orchestrator = new lambda.Function(this, 'Orchestrator', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'orchestrator.zip')),
      memorySize: 1024,
      timeout: cdk.Duration.minutes(15),
    });
    orchestrator.addToRolePolicy(s3Policy);
    orchestrator.addToRolePolicy(new iam.PolicyStatement({
      actions: ['lambda:InvokeFunction'],
      resources: [worker.functionArn],
    }));

    // Plan step
    const planTask = new tasks.LambdaInvoke(this, 'Plan', {
      lambdaFunction: planner,
      outputPath: '$.Payload',
    });

    // Worker task (invoked per item from S3 JSONL)
    const workerTask = new tasks.LambdaInvoke(this, 'ProcessDuo', {
      lambdaFunction: worker,
      outputPath: '$.Payload',
    });

    // Distributed Map reads assignments from S3 JSON array
    const mapState = new sfn.DistributedMap(this, 'FanOutWorkers', {
      maxConcurrency: 1000,
      mapExecutionType: sfn.StateMachineType.EXPRESS,
      itemReader: new sfn.S3JsonItemReader({
        bucketNamePath: sfn.JsonPath.stringAt('$.assignmentsBucket'),
        key: sfn.JsonPath.stringAt('$.assignmentsKey'),
      }),
      resultPath: '$.workerResults',
    });
    mapState.itemProcessor(workerTask);

    // Finalize step
    const finalizeTask = new tasks.LambdaInvoke(this, 'Finalize', {
      lambdaFunction: finalizer,
      payload: sfn.TaskInput.fromObject({
        uploadId: sfn.JsonPath.stringAt('$.uploadId'),
        outputBucket: sfn.JsonPath.stringAt('$.outputBucket'),
        outputKey: sfn.JsonPath.stringAt('$.outputKey'),
        cdInfoBucket: sfn.JsonPath.stringAt('$.cdInfoBucket'),
        cdInfoKey: sfn.JsonPath.stringAt('$.cdInfoKey'),
        workerResults: sfn.JsonPath.objectAt('$.workerResults'),
      }),
      outputPath: '$.Payload',
    });

    const definition = planTask.next(mapState).next(finalizeTask);

    const sm = new sfn.StateMachine(this, 'MegaZipperSM', {
      definitionBody: sfn.DefinitionBody.fromChainable(definition),
      timeout: cdk.Duration.minutes(15),
    });
    sm.addToRolePolicy(new iam.PolicyStatement({
      actions: ['s3:GetObject', 's3:PutObject', 's3:ListBucket'],
      resources: ['*'],
    }));
  }
}
