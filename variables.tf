variable "aws_region" {
  type        = string
  description = "AWS Region to deploy resources (e.g., us-east-1)"
  # No default = Must be supplied via TF_VAR_aws_region
}

variable "slack_webhook_url" {
  type        = string
  description = "Slack Webhook URL"
  sensitive   = true
  # No default = Must be supplied via TF_VAR_slack_webhook_url
}

variable "sender_email" {
  type        = string
  description = "SES Verified Sender Email"
  # No default = Must be supplied via TF_VAR_sender_email
}

variable "recipient_email" {
  type        = string
  description = "Email to receive alerts"
  # No default = Must be supplied via TF_VAR_recipient_email
}