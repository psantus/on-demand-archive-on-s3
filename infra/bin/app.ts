#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { MegaZipperStack } from '../lib/stack';

const app = new cdk.App();
new MegaZipperStack(app, 'LambdaMegaZipper');
