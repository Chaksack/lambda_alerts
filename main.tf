# main.tf

provider "aws" {
  region = var.aws_region
}

terraform {
  backend "s3" {
    bucket = "my-terraform-state-bucket" 
    key    = "ecs-alerter/terraform.tfstate"
    region = "us-east-1"
  }
}

# --- IAM Role & Policies ---
resource "aws_iam_role" "lambda_exec_role" {
  name = "ecs_alerter_lambda_role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })
}

resource "aws_iam_policy" "lambda_logging_ses" {
  name        = "ecs_alerter_policy"
  description = "IAM policy for logging and SES sending"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
        Effect   = "Allow"
        Resource = "arn:aws:logs:*:*:*"
      },
      {
        Action   = ["ses:SendEmail", "ses:SendRawEmail"]
        Effect   = "Allow"
        Resource = "*"
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "lambda_logs" {
  role       = aws_iam_role.lambda_exec_role.name
  policy_arn = aws_iam_policy.lambda_logging_ses.arn
}

# --- Lambda Function ---
resource "aws_lambda_function" "ecs_alerter" {
  filename         = "lambda_function_payload.zip"
  function_name    = "ecs-service-alerter"
  role             = aws_iam_role.lambda_exec_role.arn
  handler          = "main"
  source_code_hash = filebase64sha256("lambda_function_payload.zip")
  runtime          = "provided.al2023"
  timeout          = 10

  # Here we inject the variables into the Lambda Environment
  environment {
    variables = {
      SLACK_WEBHOOK_URL = var.slack_webhook_url
      SENDER_EMAIL      = var.sender_email
      RECIPIENT_EMAIL   = var.recipient_email
      AWS_REGION        = var.aws_region
    }
  }
}

# --- EventBridge Rules (Alert Logic) ---

# Rule 1: Deployment Failures (Rollbacks)
resource "aws_cloudwatch_event_rule" "ecs_deployment_failure" {
  name        = "ecs-deployment-failure-rule"
  description = "Capture ECS Service Deployment Failures"

  event_pattern = jsonencode({
    source      = ["aws.ecs"]
    detail-type = ["ECS Deployment State Change"]
    detail = {
      eventName = ["SERVICE_DEPLOYMENT_FAILED"]
    }
  })
}

resource "aws_cloudwatch_event_target" "target_deployment_failure" {
  rule      = aws_cloudwatch_event_rule.ecs_deployment_failure.name
  target_id = "SendToLambda"
  arn       = aws_lambda_function.ecs_alerter.arn
}

# Rule 2: Task Failures (Crashes)
resource "aws_cloudwatch_event_rule" "ecs_task_failure" {
  name        = "ecs-task-failure-rule"
  description = "Capture ECS Task Stops/Crashes"

  event_pattern = jsonencode({
    source      = ["aws.ecs"]
    detail-type = ["ECS Task State Change"]
    detail = {
      lastStatus = ["STOPPED"]
      stoppedReason = [
        "Essential container in task exited",
        "Task failed to start"
      ]
    }
  })
}

resource "aws_cloudwatch_event_target" "target_task_failure" {
  rule      = aws_cloudwatch_event_rule.ecs_task_failure.name
  target_id = "SendToLambda"
  arn       = aws_lambda_function.ecs_alerter.arn
}

# --- Permissions ---
resource "aws_lambda_permission" "allow_cloudwatch_deployment" {
  statement_id  = "AllowExecutionFromCloudWatchDeployment"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.ecs_alerter.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.ecs_deployment_failure.arn
}

resource "aws_lambda_permission" "allow_cloudwatch_task" {
  statement_id  = "AllowExecutionFromCloudWatchTask"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.ecs_alerter.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.ecs_task_failure.arn
}