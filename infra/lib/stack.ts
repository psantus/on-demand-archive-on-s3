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

    // Shared S3 policy
    const s3Policy = new iam.PolicyStatement({
      actions: ['s3:GetObject', 's3:PutObject', 's3:ListBucket',
                's3:CreateMultipartUpload', 's3:UploadPart',
                's3:CompleteMultipartUpload', 's3:AbortMultipartUpload'],
      resources: ['*'],
    });

    // Planner Lambda
    const planner = new lambda.Function(this, 'Planner', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'planner.zip')),
      memorySize: 1024,
      timeout: cdk.Duration.minutes(2),
    });
    planner.addToRolePolicy(s3Policy);

    // Worker Lambda
    const worker = new lambda.Function(this, 'Worker', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'worker.zip')),
      memorySize: 3008,
      timeout: cdk.Duration.minutes(10),
    });
    worker.addToRolePolicy(s3Policy);

    // Finalizer Lambda
    const finalizer = new lambda.Function(this, 'Finalizer', {
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(path.join(lambdaDir, 'finalizer.zip')),
      memorySize: 1024,
      timeout: cdk.Duration.minutes(2),
    });
    finalizer.addToRolePolicy(s3Policy);

    // Step Functions: Plan → Distributed Map (Workers) → Finalize
    const planTask = new tasks.LambdaInvoke(this, 'Plan', {
      lambdaFunction: planner,
      outputPath: '$.Payload',
    });

    const workerTask = new tasks.LambdaInvoke(this, 'ProcessBatch', {
      lambdaFunction: worker,
      outputPath: '$.Payload',
    });

    const mapState = new sfn.Map(this, 'FanOutWorkers', {
      itemsPath: '$.assignments',
      maxConcurrency: 5,
      resultPath: '$.workerResults',
    });
    mapState.itemProcessor(workerTask);

    const finalizeTask = new tasks.LambdaInvoke(this, 'Finalize', {
      lambdaFunction: finalizer,
      payload: sfn.TaskInput.fromObject({
        uploadId: sfn.JsonPath.stringAt('$.uploadId'),
        outputBucket: sfn.JsonPath.stringAt('$.assignments[0].outputBucket'),
        outputKey: sfn.JsonPath.stringAt('$.assignments[0].outputKey'),
        workerResults: sfn.JsonPath.objectAt('$.workerResults'),
        cdInfo: sfn.JsonPath.objectAt('$.cdInfo'),
      }),
      outputPath: '$.Payload',
    });

    const definition = planTask.next(mapState).next(finalizeTask);

    new sfn.StateMachine(this, 'MegaZipperSM', {
      definitionBody: sfn.DefinitionBody.fromChainable(definition),
      timeout: cdk.Duration.minutes(15),
    });
  }
}
